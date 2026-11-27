package linkrecovery

import "testing"

// Each outcome reads as a sentence that names the link once and says what to
// do next, and a joined summary keeps every sentence capitalised.
func TestRecordString(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record Record
		want   string
	}{
		{
			name:   "reconfigured",
			record: Record{Link: "enp1s0", Outcome: Reconfigured},
			want:   "Required link enp1s0 was reconfigured once in this boot after systemd-networkd reported no carrier while sysfs reported carrier up. No action is needed while the node is healthy",
		},
		{
			name:   "failed",
			record: Record{Link: "enp1s0", Outcome: Failed, Error: "networkctl reconfigure: exit status 1: access denied"},
			want:   "Required link enp1s0 could not be reconfigured after systemd-networkd reported no carrier while sysfs reported carrier up: networkctl reconfigure: exit status 1: access denied. Inspect 'journalctl -b -u katl-link-recovery' on the node. If the link stays offline, reboot from the node's console with 'systemctl reboot'; 'katlctl node reboot <node>' works only while another management link reaches the node",
		},
		{
			name:   "attempted",
			record: Record{Link: "enp1s0", Outcome: Attempted},
			want:   "Required link enp1s0 reconfiguration was attempted once in this boot after systemd-networkd reported no carrier while sysfs reported carrier up; its result was not recorded. Inspect 'journalctl -b -u katl-link-recovery' on the node. If the link stays offline, reboot from the node's console with 'systemctl reboot'; 'katlctl node reboot <node>' works only while another management link reaches the node",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.record.String(); got != tc.want {
				t.Fatalf("String() =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}

	summary := Summary([]Record{{Link: "enp1s0", Outcome: Reconfigured}, {Link: "enp2s0", Outcome: Attempted}})
	want := "Required link enp1s0 was reconfigured once in this boot after systemd-networkd reported no carrier while sysfs reported carrier up. No action is needed while the node is healthy. " +
		"Required link enp2s0 reconfiguration was attempted once in this boot after systemd-networkd reported no carrier while sysfs reported carrier up; its result was not recorded. Inspect 'journalctl -b -u katl-link-recovery' on the node. If the link stays offline, reboot from the node's console with 'systemctl reboot'; 'katlctl node reboot <node>' works only while another management link reaches the node"
	if summary != want {
		t.Fatalf("Summary() =\n%s\nwant\n%s", summary, want)
	}
}
