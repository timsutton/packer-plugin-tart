package tart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/bootcommand"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
	"github.com/mitchellh/go-vnc"
)

var vncRegexp = regexp.MustCompile("vnc://.*:(.*)@(.*):([0-9]{1,5})")

type stepRun struct {
	vmName string
}

type VNCBootCommandTemplateData struct {
	HTTPIP   string
	HTTPPort int
}

func (s *stepRun) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("config").(*Config)
	ui := state.Get("ui").(packersdk.Ui)

	ui.Say("Starting the virtual machine...")
	runArgs := []string{"run", config.VMName}
	if config.Headless {
		runArgs = append(runArgs, "--no-graphics")
	} else {
		runArgs = append(runArgs, "--graphics")
	}
	if !config.DisableVNC {
		runArgs = append(runArgs, "--vnc-experimental")
	}
	if config.Recovery {
		runArgs = append(runArgs, "--recovery")
	}
	cmd := exec.Command("tart", runArgs...)
	stdout := bytes.NewBufferString("")
	cmd.Stdout = stdout
	cmd.Stderr = uiWriter{ui: ui}

	// Prevent the Tart from opening the Screen Sharing
	// window connected to the VNC server we're starting
	if !config.DisableVNC {
		cmd.Env = cmd.Environ()
		cmd.Env = append(cmd.Env, "CI=true")
	}

	if err := cmd.Start(); err != nil {
		err = fmt.Errorf("Error starting VM: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	state.Put("tart-cmd", cmd)

	// HACK: bridge100 interface with IP takes a while to show up, so we just sleep
	// until we implement a proper wait loop.
	// Also, this probably would make sense to break up into separate steps so
	// that this is not all just part of the run step.
	time.Sleep(10 * time.Second)

	ip, err := ifconfig("bridge100")
	if err != nil {
		err := fmt.Errorf("Failed to parse IP from bridge100 interface: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
	}

	ui.Say(fmt.Sprintf("discovered IP: %v", ip))

	state.Put("http_ip", ip)

	if (len(config.FromISO) == 0) && !config.DisableVNC {
		if !typeBootCommandOverVNC(ctx, state, config, ui, stdout) {
			return multistep.ActionHalt
		}
	}

	ui.Say("Successfully started the virtual machine...")

	return multistep.ActionContinue
}

type uiWriter struct {
	ui packersdk.Ui
}

func (u uiWriter) Write(p []byte) (n int, err error) {
	u.ui.Say(string(p))
	return len(p), nil
}

// Cleanup stops the VM.
func (s *stepRun) Cleanup(state multistep.StateBag) {
	config := state.Get("config").(*Config)
	ui := state.Get("ui").(packersdk.Ui)

	communicator := state.Get("communicator")
	if communicator != nil {
		ui.Say("Gracefully shutting down the VM...")

		shutdownCmd := packersdk.RemoteCmd{
			Command: fmt.Sprintf("echo %s | sudo -S shutdown -h now", config.Comm.Password()),
		}

		err := shutdownCmd.RunWithUi(context.Background(), communicator.(packersdk.Communicator), ui)
		if err != nil {
			ui.Say("Failed to gracefully shutdown VM...")
			ui.Error(err.Error())
		}
	}

	cmd := state.Get("tart-cmd").(*exec.Cmd)

	if cmd != nil {
		ui.Say("Waiting for the tart process to exit...")
		_, _ = cmd.Process.Wait()
	}
}

func typeBootCommandOverVNC(
	ctx context.Context,
	state multistep.StateBag,
	config *Config,
	ui packersdk.Ui,
	tartRunStdout *bytes.Buffer,
) bool {
	ui.Say("Waiting for the VNC server credentials from Tart...")

	var vncPassword string
	var vncHost string
	var vncPort string

	for {
		matches := vncRegexp.FindStringSubmatch(tartRunStdout.String())
		if len(matches) == 1+vncRegexp.NumSubexp() {
			vncPassword = matches[1]
			vncHost = matches[2]
			vncPort = matches[3]

			break
		}

		time.Sleep(time.Second)
	}

	ui.Say("Retrieved VNC credentials, connecting...")

	netConn, err := net.Dial("tcp", fmt.Sprintf("%s:%s", vncHost, vncPort))
	if err != nil {
		err := fmt.Errorf("Failed to connect to the Tart's VNC server: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())

		return false
	}
	defer netConn.Close()

	vncClient, err := vnc.Client(netConn, &vnc.ClientConfig{
		Auth: []vnc.ClientAuth{
			&vnc.PasswordAuth{Password: vncPassword},
		},
	})
	if err != nil {
		err := fmt.Errorf("Failed to connect to the Tart's VNC server: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())

		return false
	}
	defer vncClient.Close()

	ui.Say("Connected to the VNC!")

	vncDriver := bootcommand.NewVNCDriver(vncClient, config.BootKeyInterval)

	ui.Say("Typing the commands over VNC...")

	hostIP := state.Get("http_ip").(string)
	httpPort := state.Get("http_port").(int)
	config.ctx.Data = &VNCBootCommandTemplateData{
		HTTPIP:   hostIP,
		HTTPPort: httpPort,
	}

	command, err := interpolate.Render(config.VNCConfig.FlatBootCommand(), &config.ctx)

	if err != nil {
		err := fmt.Errorf("Failed to render the boot command: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())

		return false
	}

	seq, err := bootcommand.GenerateExpressionSequence(command)
	if err != nil {
		err := fmt.Errorf("Failed to parse the boot command: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())

		return false
	}

	if err := seq.Do(ctx, vncDriver); err != nil {
		err := fmt.Errorf("Failed to run the boot command: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())

		return false
	}

	return true
}

// Lifted from the vmware builder (reduced since we can assume this builder
// is only ever used on Darwin, and thus use consistent executable paths)
func ifconfig(device string) (string, error) {
	ifconfigPath := "/sbin/ifconfig"

	stdout := new(bytes.Buffer)

	cmd := exec.Command(ifconfigPath, device)
	// Force LANG=C so that the output is what we expect it to be
	// despite the locale.
	cmd.Env = append(cmd.Env, "LANG=C")
	cmd.Env = append(cmd.Env, os.Environ()...)

	cmd.Stdout = stdout
	cmd.Stderr = new(bytes.Buffer)
	if err := cmd.Run(); err != nil {
		return "", err
	}

	re := regexp.MustCompile(`inet[^\d]+([\d\.]+)\s`)
	matches := re.FindStringSubmatch(stdout.String())
	if matches == nil {
		return "", errors.New("IP not found in ifconfig output...")
	}

	return matches[1], nil
}
