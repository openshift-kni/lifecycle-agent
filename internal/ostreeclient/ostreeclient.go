package ostreeclient

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

//go:generate mockgen -source=ostreeclient.go -package=ostreeclient -destination=mock_ostreeclient.go
type IClient interface {
	PullLocal(ctx context.Context, repoPath string) error
	OSInit(ctx context.Context, osname string) error
	Deploy(ctx context.Context, osname, refsepc string, kargs []string, rpmOstreeClient rpmostreeclient.IClient, ibi bool) error
	Undeploy(ctx context.Context, ostreeIndex int) error
	SetDefaultDeployment(ctx context.Context, index int) error
	IsOstreeAdminSetDefaultFeatureEnabled(ctx context.Context) (bool, error)
	GetDeployment(ctx context.Context, osname string) (string, error)
	GetDeploymentDir(ctx context.Context, osname string) (string, error)
}

type Client struct {
	executor ops.Execute
	ibi      bool
}

func NewClient(executor ops.Execute, ibi bool) IClient {
	return &Client{
		executor: executor,
		ibi:      ibi,
	}
}

func (c *Client) PullLocal(ctx context.Context, repoPath string) error {
	args := []string{"pull-local"}
	if c.ibi {
		args = append(args, "--repo", "/mnt/ostree/repo")
	}
	if _, err := c.executor.Execute(ctx, "ostree", append(args, repoPath)...); err != nil {
		return fmt.Errorf("failed to pull local ostree with args %s, %w", args, err)
	}

	return nil
}

func (c *Client) OSInit(ctx context.Context, osname string) error {
	args := []string{"admin", "os-init"}
	if c.ibi {
		args = append(args, "--sysroot", "/mnt")
	}

	if _, err := c.executor.Execute(ctx, "ostree", append(args, osname)...); err != nil {
		return fmt.Errorf("failed to run OSInit with args %s: %w", args, err)
	}
	return nil
}

func (c *Client) Deploy(ctx context.Context, osname, refsepc string, kargs []string, rpmOstreeClient rpmostreeclient.IClient, ibi bool) error {
	args := []string{"admin", "deploy", "--os", osname, "--no-prune"}
	if c.ibi {
		args = append(args, "--sysroot", "/mnt")
	}
	args = append(args, kargs...)
	args = append(args, refsepc)
	if enabled, _ := c.IsOstreeAdminSetDefaultFeatureEnabled(ctx); !c.ibi && enabled {
		args = append(args, "--not-as-default")
	}

	// Run the command in bash to preserve the quoted kargs
	args = append([]string{"ostree"}, args...)
	if _, err := c.executor.Execute(ctx, "bash", "-c", strings.Join(args, " ")); err != nil {
		return fmt.Errorf("failed to run OSInit with args %s: %w", args, err)
	}

	if !ibi {
		// In an IBU where both releases have the same underlying rhcos image, the parent commit of the deployment has
		// unique commit IDs (due to import from seed), but the same checksum. In order to avoid pruning the original parent
		// commit and corrupting the ostree, the previous "ostree admin deploy" command was called with the "--no-prune" option.
		// This must also be followed up with a call to "rpm-ostree cleanup -b" to update the base refs.
		if err := rpmOstreeClient.RpmOstreeCleanup(ctx); err != nil {
			return fmt.Errorf("failed rpm-ostree cleanup -b: %w", err)
		}
	}

	return nil
}

func (c *Client) Undeploy(ctx context.Context, ostreeIndex int) error {
	args := []string{"admin", "undeploy"}
	if c.ibi {
		args = append(args, "--sysroot", "/mnt")
	}
	args = append(args, fmt.Sprint(ostreeIndex))
	if _, err := c.executor.Execute(ctx, "ostree", args...); err != nil {
		return fmt.Errorf("failed to run Undeploy with args %s: %w", args, err)
	}
	return nil
}

func (c *Client) IsOstreeAdminSetDefaultFeatureEnabled(ctx context.Context) (bool, error) {
	// Quick check to see if the "ostree admin set-default" feature is available
	output, err := c.executor.Execute(ctx, "ostree", "admin", "--help")
	if err != nil {
		return false, fmt.Errorf("failed to probe ostree admin capabilities: %w", err)
	}

	return strings.Contains(output, "set-default"), nil
}

func (c *Client) SetDefaultDeployment(ctx context.Context, index int) error {
	if index == 0 {
		// Already set as default deployment
		return nil
	}

	args := []string{"admin", "set-default", strconv.Itoa(index)}
	if _, err := c.executor.Execute(ctx, "ostree", args...); err != nil {
		return fmt.Errorf("failed run ostree set-default with args %s: %w", args, err)
	}

	return nil
}

func (c *Client) GetDeployment(ctx context.Context, stateroot string) (string, error) {
	args := []string{"admin", "status"}
	if c.ibi {
		args = append(args, "--sysroot", common.OstreeDeployPathPrefix)
	}

	output, err := c.executor.Execute(ctx, "ostree", args...)
	if err != nil {
		return "", fmt.Errorf("unable to get deployment, ostree command failed: %w", err)
	}

	// Example output:
	//   # ostree admin status
	//   * rhcos 9455b99374197f10c453eb96f1b66cea884b3dc16ce4bc753bdb7263602bb722.0
	//       origin: <unknown origin type>
	//     rhcos_4.15.0_rc.1 8ef186bc6407db2180726e32354c394c189c6e9be2c17839b313cf1fed3d5391.0
	//       origin: <unknown origin type>
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		if fields[0] == "*" {
			// Pop off the *, which indicates the currently booted deployment
			fields = fields[1:]
		}

		if len(fields) < 2 {
			continue
		}

		if fields[0] == stateroot {
			// Return the deployment for the first matching stateroot
			return fields[1], nil
		}
	}

	return "", nil
}

func (c *Client) GetDeploymentDir(ctx context.Context, stateroot string) (string, error) {
	deployment, err := c.GetDeployment(ctx, stateroot)
	if err != nil {
		return "", fmt.Errorf("unable to get determine deployment dir: %w", err)
	}

	deploymentDir := filepath.Join(common.GetStaterootPath(stateroot), "deploy", deployment)
	return deploymentDir, nil
}
