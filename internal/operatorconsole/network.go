package operatorconsole

import (
	"context"
	"encoding/json"
	"os/exec"
)

func readInterfaceVRFs(ctx context.Context) (map[string]string, error) {
	data, err := exec.CommandContext(ctx, "ip", "-j", "-d", "link", "show").Output()
	if err != nil {
		return nil, err
	}
	return decodeInterfaceVRFs(data)
}

func decodeInterfaceVRFs(data []byte) (map[string]string, error) {
	type link struct {
		Name   string `json:"ifname"`
		Master string `json:"master"`
		Info   struct {
			Kind string `json:"info_kind"`
		} `json:"linkinfo"`
	}
	var links []link
	if err := json.Unmarshal(data, &links); err != nil {
		return nil, err
	}
	byName := make(map[string]link, len(links))
	for _, item := range links {
		byName[item.Name] = item
	}
	vrfs := make(map[string]string)
	for _, item := range links {
		master := item.Master
		// Follow bridge/bond masters too, bounded against malformed cycles.
		for range len(links) {
			parent, ok := byName[master]
			if !ok {
				break
			}
			if parent.Info.Kind == "vrf" {
				vrfs[item.Name] = parent.Name
				break
			}
			master = parent.Master
		}
	}
	return vrfs, nil
}
