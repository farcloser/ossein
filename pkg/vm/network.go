//go:build darwin && arm64

package vm

import "github.com/forkcloser/vz/vmnet"

// Network is the vmnet network created for ONE microVM, plus the addressing
// the guest needs to use it: vmnet.PerVM, whose documentation says why a
// per-VM network and not the framework's shared NAT.
//
// Because ossein chose the network, the guest's address is known host-side
// before the VM starts and is applied statically over the netlink RPCs, about
// a millisecond, against the 85-300ms a DHCP lease cost.
type Network = vmnet.PerVM

func newNetwork() (*Network, error) {
	return vmnet.NewPerVM()
}
