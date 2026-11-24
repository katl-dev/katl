package vmtest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The interrupted lines are trimmed from merge-group runs 45298999344 and
// 45280682142, where kernel records split the system-disk refusal.
const splitRefusal = "[    4.393469] cat[476]: KatlOS could not identify its system di[    4.803472] audit: type=1130 audit(1795485281.270:5): msg='unit=systemd-network-generator'\r\n" +
	"sk: vda1 on disk[    4.809013] audit: type=1130 audit(1795485281.273:6): msg='unit=systemd-remount-fs'\r\n" +
	" vda has the same PARTUUID as vdc1.\r\n"

func TestConsoleText(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "records split one line twice",
			raw:  splitRefusal,
			want: "[    4.803472] audit: type=1130 audit(1795485281.270:5): msg='unit=systemd-network-generator'\r\n" +
				"[    4.809013] audit: type=1130 audit(1795485281.273:6): msg='unit=systemd-remount-fs'\r\n" +
				"[    4.393469] cat[476]: KatlOS could not identify its system disk: vda1 on disk vda has the same PARTUUID as vdc1.\r\n",
		},
		{
			name: "records split the journal prefix",
			raw: "[    3.966901] c[    4.318655] IPVS: Registered protocols (TCP, UDP, SCTP, AH, ESP)\r\n" +
				"at[454]: KatlOS could not identi[    4.321438] audit: type=1130 audit(1795475942.428:6): msg='unit=systemd-tmpfiles-setup-dev-early'\r\n" +
				"fy its system disk: vdc1 on disk vdc.\r\n",
			want: "[    4.318655] IPVS: Registered protocols (TCP, UDP, SCTP, AH, ESP)\r\n" +
				"[    4.321438] audit: type=1130 audit(1795475942.428:6): msg='unit=systemd-tmpfiles-setup-dev-early'\r\n" +
				"[    3.966901] cat[454]: KatlOS could not identify its system disk: vdc1 on disk vdc.\r\n",
		},
		{
			name: "records back to back",
			raw: "[    4.374359] systemd[1]: Mounted sys-kernel-tr[    4.783985] audit: type=1130 msg='unit=kmod-static-nodes'\r\n" +
				"[    4.789319] audit: type=1130 msg='unit=lvm2-monitor'\r\n" +
				"acing.mount - Kernel Trace File System.\r\n",
			want: "[    4.783985] audit: type=1130 msg='unit=kmod-static-nodes'\r\n" +
				"[    4.789319] audit: type=1130 msg='unit=lvm2-monitor'\r\n" +
				"[    4.374359] systemd[1]: Mounted sys-kernel-tracing.mount - Kernel Trace File System.\r\n",
		},
		{
			name: "uninterrupted lines",
			raw: "[    1.000000] systemd[1]: Started x.\r\n" +
				"[  OK  ] Reached target y.\r\n" +
				"cat[476]: index[1] = 2, uptime [ 1.5] \r\n" +
				"plain\r\n",
			want: "[    1.000000] systemd[1]: Started x.\r\n" +
				"[  OK  ] Reached target y.\r\n" +
				"cat[476]: index[1] = 2, uptime [ 1.5] \r\n" +
				"plain\r\n",
		},
		{
			name: "remainder not yet written",
			raw:  "[    4.393469] cat[476]: KatlOS could not ide[    4.803472] audit: type=1130\r\n",
			want: "[    4.803472] audit: type=1130\r\n[    4.393469] cat[476]: KatlOS could not ide",
		},
		{
			name: "unterminated record",
			raw:  "[    4.393469] cat[476]: KatlOS could not ide[    4.803472] aud",
			want: "[    4.393469] cat[476]: KatlOS could not ide[    4.803472] aud",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := consoleText(tt.raw); got != tt.want {
				t.Fatalf("consoleText() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestWaitForSerialSignalAcrossKernelRecords(t *testing.T) {
	serialLog := filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(serialLog, []byte(splitRefusal), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := waitForSerialSignal(ctx, make(chan struct{}), nil, serialLog, "could not identify its system disk: vda1 on disk vda has", 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
}
