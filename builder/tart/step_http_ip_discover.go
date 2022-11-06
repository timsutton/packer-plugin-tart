package tart

import (
	"context"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
)

// Step to discover the http ip
// TODO: Put more context in comments here
type stepHTTPIPDiscover struct{}

func (s *stepHTTPIPDiscover) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	// driver := state.Get("driver").(Driver)
	// ui := state.Get("ui").(packersdk.Ui)

	// Copy-pasted from vmware builder common
	// Whatever method we use to derive the IP, at least it'll only need
	// to work on Darwin.
	//
	// // Determine the host IP
	// hostIP, err := driver.HostIP(state)
	// if err != nil {
	// 	err := fmt.Errorf("Error detecting host IP: %s", err)
	// 	state.Put("error", err)
	// 	ui.Error(err.Error())
	// 	return multistep.ActionHalt
	// }

	// log.Printf("Host IP for the VMware machine: %s", hostIP)

	hardcodedIp := "192.168.1.220"
	// ui.Say(fmt.Sprintf("DEBUG: We will fill in the host IP here to %v", hardcodedIp))

	state.Put("http_ip", hardcodedIp)

	return multistep.ActionContinue
}

func (*stepHTTPIPDiscover) Cleanup(multistep.StateBag) {}
