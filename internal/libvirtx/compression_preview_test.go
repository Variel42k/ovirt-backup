package libvirtx

import "testing"

func TestCompressionBackupOwnershipIsExactXMLIdentity(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		owned bool
	}{
		{`<domainbackup><server socket='/scratch/owned.sock'/></domainbackup>`, true},
		{`<domainbackup><server socket='/scratch/owned.sock-other'/></domainbackup>`, false},
		{`<domainbackup><description>/scratch/owned.sock</description><server socket='/scratch/foreign.sock'/></domainbackup>`, false},
		{`not XML /scratch/owned.sock`, false},
	} {
		if got := CompressionBackupOwned(tc.raw, "/scratch/owned.sock"); got != tc.owned {
			t.Fatalf("ownership %q = %v", tc.raw, got)
		}
	}
}
