//go:build darwin && arm64

package vm //nolint:testpackage // addressing is unexported by design

import (
	"net/netip"
	"testing"
)

// TestAddressing pins how the framework's reported subnet maps to the two
// addresses ossein uses. The shape that matters is the one vmnet actually
// returns: the prefix carries the GATEWAY's address, not the network base.
func TestAddressing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		prefix  string
		gateway string
		guest   string
		wantErr bool
	}{
		// What vmnet reports in practice.
		{"192.168.64.1/24", "192.168.64.1", "192.168.64.2", false},
		{"192.168.65.1/24", "192.168.65.1", "192.168.65.2", false},
		// Network base reported instead: derive the first host address.
		{"192.168.64.0/24", "192.168.64.1", "192.168.64.2", false},
		// A gateway that is not the first host address must be honored, not
		// "corrected" — that is the difference between faithful and lucky.
		{"192.168.64.9/24", "192.168.64.9", "192.168.64.10", false},
		{"10.1.2.1/23", "10.1.2.1", "10.1.2.2", false},
		// No room for a guest beside the gateway.
		{"192.168.64.1/31", "", "", true},
	}

	for _, testCase := range cases {
		prefix := netip.MustParsePrefix(testCase.prefix)

		gateway, guest, err := addressing(prefix)
		if (err != nil) != testCase.wantErr {
			t.Fatalf("addressing(%s) err = %v, wantErr %v", testCase.prefix, err, testCase.wantErr)
		}

		if testCase.wantErr {
			continue
		}

		if gateway.String() != testCase.gateway || guest.String() != testCase.guest {
			t.Errorf("addressing(%s) = (%s, %s), want (%s, %s)",
				testCase.prefix, gateway, guest, testCase.gateway, testCase.guest)
		}

		if !prefix.Contains(guest) {
			t.Errorf("addressing(%s): guest %s outside the subnet", testCase.prefix, guest)
		}
	}
}
