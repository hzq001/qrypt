package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/spf13/cobra"
	clicore "github.com/yinzhenyu/qrypt/internal/cli/core"
	"github.com/yinzhenyu/qrypt/internal/cli/i18n"
	cliruntime "github.com/yinzhenyu/qrypt/internal/cli/runtime"
	"github.com/yinzhenyu/qrypt/pkg/control"
	"github.com/yinzhenyu/qrypt/pkg/logging"
	"github.com/yinzhenyu/qrypt/pkg/util"
)

const attachPollInterval = 2 * time.Second

type attachMountClient interface {
	ListMounts(ctx context.Context) ([]control.MountStatus, error)
	MountOutcome(ctx context.Context, id string) (control.MountOutcome, error)
}

// runAttach asks the resident Core to host the FUSE session instead of opening
// a second filesystem in this process. Ctrl+C detaches the session but leaves
// the Core (and every cache) alive for the other clients.
func runAttach(rt cliruntime.Runtime, cmd *cobra.Command, state cliruntime.ConfigState, ctx context.Context, selectedMount, mountPointFlag string, stopCore bool) error {
	loc := i18n.FromCommand(cmd)
	mountPoint := mountPointFlag
	if mountPoint == "" {
		var err error
		mountPoint, err = rt.MountPointFromConfig(state.Cfg)
		if err != nil {
			return err
		}
	}
	mountPoint = util.ExpandHome(mountPoint)

	logging.L.Infof("Mounting at %s ...", mountPoint)
	fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", loc.T("out.mount.mounting", mountPoint))
	client, err := clicore.EnsureClient(ctx, rt, state)
	if err != nil {
		return err
	}
	attachCtx, cancelAttach := context.WithTimeout(ctx, 30*time.Second)
	defer cancelAttach()
	info, err := client.AttachMount(attachCtx, control.MountAttachRequest{
		Name:       selectedMount,
		MountPoint: mountPoint,
	})
	if err != nil {
		return err
	}

	var detachOnce sync.Once
	detach := func() {
		detachOnce.Do(func() {
			detachCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			logging.L.Infof("Unmounting %s ...", mountPoint)
			fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", loc.T("out.mount.unmounting", mountPoint))
			if err := client.DetachMount(detachCtx, info.ID); err != nil {
				logging.L.Warnf("Unmount failed: %v", err)
			}
			if stopCore {
				if _, err := client.PostJSON(detachCtx, "/v1/control/shutdown", map[string]bool{"drain": true}); err != nil {
					logging.L.Warnf("Stop core failed: %v", err)
				}
			}
		})
	}
	defer detach()

	logging.L.Infof("Mounted at %s. Press Ctrl+C to unmount.", mountPoint)
	fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", loc.T("out.mount.mounted", mountPoint))

	return watchAttachedMount(
		ctx,
		client,
		info.ID,
		mountPoint,
		attachPollInterval,
		detach,
		func() { detachOnce.Do(func() {}) },
		cmd.ErrOrStderr(),
	)
}

func watchAttachedMount(ctx context.Context, client attachMountClient, id, mountPoint string, pollInterval time.Duration, detach, consumeDetach func(), errOut io.Writer) error {
	if pollInterval <= 0 {
		pollInterval = attachPollInterval
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			detach()
			return nil
		case <-ticker.C:
			listCtx, cancelList := context.WithTimeout(context.Background(), 5*time.Second)
			mounts, err := client.ListMounts(listCtx)
			cancelList()
			if err != nil {
				logging.L.Warnf("Mount stopped unexpectedly: %v", err)
				detach()
				return fmt.Errorf("%s: %w", mountPoint, errMountSessionEnded)
			}
			found := false
			for _, hosted := range mounts {
				if hosted.ID == id {
					found = true
					break
				}
			}
			if !found {
				outcomeCtx, cancelOutcome := context.WithTimeout(context.Background(), 5*time.Second)
				outcome, outcomeErr := client.MountOutcome(outcomeCtx, id)
				cancelOutcome()
				if outcomeErr != nil {
					var transportErr *control.TransportError
					if errors.As(outcomeErr, &transportErr) && (transportErr.Status == http.StatusNotFound || transportErr.Status == http.StatusNotImplemented) {
						consumeDetach()
						logging.L.Infof("Mount %s was detached by another client", mountPoint)
						return nil
					}
					consumeDetach()
					logging.L.Warnf("Mount outcome unavailable for %s: %v", mountPoint, outcomeErr)
					return fmt.Errorf("%s: %w", mountPoint, errMountSessionEnded)
				}
				switch outcome.Reason {
				case "external":
					// Something outside qrypt ejected the volume (Finder eject,
					// `diskutil unmount`, `umount`). Consume detach and exit 0 so
					// launchd does not remount a volume the user just ejected.
					consumeDetach()
					logging.L.Infof("Unmounted externally: %s", mountPoint)
					fmt.Fprintf(errOut, "Unmounted externally: %s\n", mountPoint)
					return nil
				case "unexpected":
					// The FUSE session ended while the mount remained registered
					// (or died with a session error). Consume detach so we do not
					// try to detach a dead session again, and return non-zero so
					// a service manager can restart and clear the stale mount.
					consumeDetach()
					logging.L.Warnf("FUSE session ended unexpectedly mount=%q err=%s", mountPoint, outcome.Error)
					if outcome.Error != "" {
						fmt.Fprintf(errOut, "FUSE session ended unexpectedly: %s\n", outcome.Error)
					}
					return fmt.Errorf("%s: %w", mountPoint, errMountSessionEnded)
				default:
					// Another client detached the volume (qrypt unmount); this is a
					// normal end of the attach session, so consume detach and exit 0.
					consumeDetach()
					logging.L.Infof("Mount %s was detached by another client", mountPoint)
					return nil
				}
			}
		}
	}
}
