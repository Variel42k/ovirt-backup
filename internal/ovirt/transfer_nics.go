package ovirt

import (
	"context"
	"fmt"
	"time"
)

type TransferNIC struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MAC  struct {
		Address string `json:"address"`
	} `json:"mac"`
}

func (c *Client) TransferNICs(ctx context.Context, vmID string) ([]TransferNIC, error) {
	var list struct {
		NIC []TransferNIC `json:"nic"`
	}
	err := c.get(ctx, "/vms/"+vmID+"/nics", &list)
	return list.NIC, err
}

func (c *Client) SetTransferNIC(ctx context.Context, vmID, nicID string, connected bool) error {
	return c.put(ctx, fmt.Sprintf("/vms/%s/nics/%s", vmID, nicID), map[string]any{"linked": connected, "on_boot": connected}, nil)
}

func (c *Client) WaitTransferVMDeleted(ctx context.Context, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		_, err := c.GetVMState(ctx, id)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
