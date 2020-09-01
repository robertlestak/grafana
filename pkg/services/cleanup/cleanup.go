package cleanup

import (
	"context"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"io/ioutil"
	"os"
	"path"
	"time"

	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/serverlock"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/registry"
	"github.com/grafana/grafana/pkg/setting"
)

type CleanUpService struct {
	log               log.Logger
	Cfg               *setting.Cfg                  `inject:""`
	ServerLockService *serverlock.ServerLockService `inject:""`
	SQLStore          *sqlstore.SqlStore            `inject:""`
}

func init() {
	registry.RegisterService(&CleanUpService{})
}

func (srv *CleanUpService) Init() error {
	srv.log = log.New("cleanup")
	return nil
}

func (srv *CleanUpService) Run(ctx context.Context) error {
	srv.cleanUpTmpFiles()

	ticker := time.NewTicker(time.Minute * 10)
	for {
		select {
		case <-ticker.C:
			srv.cleanUpTmpFiles()
			srv.deleteExpiredSnapshots()
			srv.deleteExpiredDashboardVersions()
			srv.deleteExpiredUserInvites(ctx)
			err := srv.ServerLockService.LockAndExecute(ctx, "delete old login attempts",
				time.Minute*10, func() {
					srv.deleteOldLoginAttempts()
				})
			if err != nil {
				srv.log.Error("failed to lock and execute cleanup of old login attempts", "error", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (srv *CleanUpService) cleanUpTmpFiles() {
	if _, err := os.Stat(srv.Cfg.ImagesDir); os.IsNotExist(err) {
		return
	}

	files, err := ioutil.ReadDir(srv.Cfg.ImagesDir)
	if err != nil {
		srv.log.Error("Problem reading image dir", "error", err)
		return
	}

	var toDelete []os.FileInfo
	var now = time.Now()

	for _, file := range files {
		if srv.shouldCleanupTempFile(file.ModTime(), now) {
			toDelete = append(toDelete, file)
		}
	}

	for _, file := range toDelete {
		fullPath := path.Join(srv.Cfg.ImagesDir, file.Name())
		err := os.Remove(fullPath)
		if err != nil {
			srv.log.Error("Failed to delete temp file", "file", file.Name(), "error", err)
		}
	}

	srv.log.Debug("Found old rendered image to delete", "deleted", len(toDelete), "kept", len(files))
}

func (srv *CleanUpService) shouldCleanupTempFile(filemtime time.Time, now time.Time) bool {
	if srv.Cfg.TempDataLifetime == 0 {
		return false
	}

	return filemtime.Add(srv.Cfg.TempDataLifetime).Before(now)
}

func (srv *CleanUpService) deleteExpiredSnapshots() {
	cmd := models.DeleteExpiredSnapshotsCommand{}
	if err := bus.Dispatch(&cmd); err != nil {
		srv.log.Error("Failed to delete expired snapshots", "error", err.Error())
	} else {
		srv.log.Debug("Deleted expired snapshots", "rows affected", cmd.DeletedRows)
	}
}

func (srv *CleanUpService) deleteExpiredDashboardVersions() {
	cmd := models.DeleteExpiredVersionsCommand{}
	if err := bus.Dispatch(&cmd); err != nil {
		srv.log.Error("Failed to delete expired dashboard versions", "error", err.Error())
	} else {
		srv.log.Debug("Deleted old/expired dashboard versions", "rows affected", cmd.DeletedRows)
	}
}

func (srv *CleanUpService) deleteOldLoginAttempts() {
	if srv.Cfg.DisableBruteForceLoginProtection {
		return
	}

	cmd := models.DeleteOldLoginAttemptsCommand{
		OlderThan: time.Now().Add(time.Minute * -10),
	}
	if err := bus.Dispatch(&cmd); err != nil {
		srv.log.Error("Problem deleting expired login attempts", "error", err.Error())
	} else {
		srv.log.Debug("Deleted expired login attempts", "rows affected", cmd.DeletedRows)
	}
}

func (srv *CleanUpService) deleteExpiredUserInvites(ctx context.Context) (int64, error) {
	maxInviteLifetime := time.Duration(srv.Cfg.UserInviteMaxLifetimeDays) * 24 * time.Hour

	var affected int64
	err := srv.SQLStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		sql := `DELETE from temp_user WHERE created_at <= ?`
		createdBefore := time.Now().Add(-maxInviteLifetime)

		srv.log.Debug("starting cleanup of expired user invites", "createdBefore", createdBefore)

		res, err := dbSession.Exec(sql, createdBefore.Unix())
		if err != nil {
			return err
		}

		affected, err = res.RowsAffected()
		if err != nil {
			srv.log.Error("failed to cleanup expired user invites", "error", err)
			return nil
		}

		srv.log.Debug("cleanup of expired user invites done", "count", affected)

		return nil
	})

	return affected, err
}
