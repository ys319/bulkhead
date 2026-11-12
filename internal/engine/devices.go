package engine

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// TagImage gives an image another name.
func (c *Client) TagImage(ctx context.Context, source, target string) error {
	_, err := c.api.ImageTag(ctx, client.ImageTagOptions{Source: source, Target: target})
	return err
}

// CDIDevices are the CDI devices the engine found. The engine is asked, not
// the host's /etc/cdi: with Docker Desktop the engine lives in a VM and
// sees different files. known is false when the engine does not report
// devices at all (before Engine 28.2, or with CDI turned off).
func (c *Client) CDIDevices(ctx context.Context) (devices []string, known bool, err error) {
	result, err := c.api.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, false, err
	}
	if result.Info.DiscoveredDevices == nil {
		return nil, false, nil
	}
	devices = []string{}
	for _, device := range result.Info.DiscoveredDevices {
		if device.Source == "cdi" {
			devices = append(devices, device.ID)
		}
	}
	return devices, true, nil
}

// DeviceGIDs puts the CDI devices into a one-off container of the image and
// returns the groups dev's user has to join to use them. Doing it for real
// also finds devices that are listed but cannot be injected (a spec left
// behind by hardware that is gone), before the environment's own containers
// are touched.
func (c *Client) DeviceGIDs(ctx context.Context, image string, devices []string) ([]int, error) {
	out, err := c.runOnce(ctx,
		&container.Config{
			Image:      image,
			Entrypoint: []string{"find"},
			Cmd:        []string{"/dev", "-type", "c", "-exec", "stat", "-c", "%g %a %n", "{}", ";"},
		},
		&container.HostConfig{
			NetworkMode: "none",
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges:true"},
			Resources: container.Resources{
				DeviceRequests: []container.DeviceRequest{{Driver: "cdi", DeviceIDs: devices}},
			},
		})
	if err != nil {
		return nil, fmt.Errorf("the CDI devices %s could not be put into a container: %w", strings.Join(devices, ", "), err)
	}
	return ParseDeviceProbe(out), nil
}

var deviceProbeLine = regexp.MustCompile(`^(\d+) ([0-7]{3,4}) (.+)$`)

// ParseDeviceProbe reads `stat -c '%g %a %n'` lines and returns the groups
// of the device nodes that are not readable and writable by everyone. Group
// 0 is left out: the container's own nodes are root:root 0666, and handing
// out the root group for a 0660 root node would be too much.
func ParseDeviceProbe(output string) []int {
	var gids []int
	for _, line := range strings.Split(output, "\n") {
		matched := deviceProbeLine.FindStringSubmatch(strings.TrimSpace(line))
		if matched == nil {
			continue
		}
		gid, _ := strconv.Atoi(matched[1])
		mode, _ := strconv.ParseInt(matched[2], 8, 32)
		if gid != 0 && mode&0o006 != 0o006 && !slices.Contains(gids, gid) {
			gids = append(gids, gid)
		}
	}
	slices.Sort(gids)
	return gids
}
