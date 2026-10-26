package db

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

func createUserAdminTestActor(t *testing.T, store *Store, name string) *models.User {
	t.Helper()
	actor := &models.User{
		Name: name, PassHash: "hash", Salt: "salt",
		Role: models.RoleAdmin, Status: models.StatusActive,
	}
	if err := store.CreateUser(actor); err != nil {
		t.Fatalf("create admin actor: %v", err)
	}
	return actor
}

func userAdminStringPtr(value string) *string {
	return &value
}

func userAdminBoolPtr(value bool) *bool {
	return &value
}

func TestUpdateUserAdminCommitsFieldsAndRevocationsTogether(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	actor := createUserAdminTestActor(t, store, "admin-actor")
	target := addAuthAtomicFixture(t, store, "admin-target", now)

	updated, err := store.UpdateUserAdmin(AdminUserUpdate{
		ActorID:       actor.ID,
		UserID:        target.playback.user.ID,
		Name:          userAdminStringPtr("admin-target-renamed"),
		Role:          userAdminStringPtr(models.RoleAdmin),
		Status:        userAdminStringPtr(models.StatusDisabled),
		Email:         userAdminStringPtr("target@example.com"),
		EmailVerified: userAdminBoolPtr(true),
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("update user: %v", err)
	}
	if updated.Name != "admin-target-renamed" ||
		updated.Role != models.RoleAdmin ||
		updated.Status != models.StatusDisabled ||
		updated.Email != "target@example.com" ||
		!updated.EmailVerified {
		t.Fatalf("updated user = %+v", updated)
	}

	var epoch int64
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		target.playback.user.ID).Scan(&epoch); err != nil {
		t.Fatalf("read auth epoch: %v", err)
	}
	if epoch != target.epoch+1 {
		t.Errorf("auth epoch = %d, want %d", epoch, target.epoch+1)
	}
	assertAuthAtomicRevoked(t, store, target, now.Add(time.Minute))
	if n, err := store.ActiveAdminCount(); err != nil || n != 1 {
		t.Fatalf("active admins after disabling target = %d, err %v", n, err)
	}
}

func TestUpdateUserAdminRollsBackLateRevocationFailure(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	actor := createUserAdminTestActor(t, store, "admin-rollback-actor")
	target := addAuthAtomicFixture(t, store, "admin-rollback-target", now)

	if _, err := store.DB.Exec(`CREATE TRIGGER injected_admin_update_failure
		BEFORE UPDATE ON device_refresh_tokens
		BEGIN
			SELECT RAISE(ABORT, 'injected admin update failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	_, err := store.UpdateUserAdmin(AdminUserUpdate{
		ActorID:       actor.ID,
		UserID:        target.playback.user.ID,
		Name:          userAdminStringPtr("must-not-stick"),
		Status:        userAdminStringPtr(models.StatusDisabled),
		Email:         userAdminStringPtr("must-not-stick@example.com"),
		EmailVerified: userAdminBoolPtr(true),
	}, now.Add(time.Minute))
	if err == nil {
		t.Fatal("update unexpectedly succeeded despite injected revocation failure")
	}

	user, err := store.UserByID(target.playback.user.ID)
	if err != nil {
		t.Fatalf("read user after failed update: %v", err)
	}
	if user.Name != target.playback.user.Name ||
		user.Role != target.playback.user.Role ||
		user.Status != target.playback.user.Status ||
		user.Email != target.playback.user.Email ||
		user.EmailVerified != target.playback.user.EmailVerified {
		t.Errorf("user changed after failed update: %+v", user)
	}
	var epoch int64
	if err := store.DB.QueryRow(`SELECT auth_epoch FROM users WHERE id = ?`,
		target.playback.user.ID).Scan(&epoch); err != nil {
		t.Fatalf("read auth epoch after failed update: %v", err)
	}
	if epoch != target.epoch {
		t.Errorf("auth epoch after failed update = %d, want %d", epoch, target.epoch)
	}
	assertAuthAtomicActive(t, store, target, now)
}

func TestUpdateUserAdminKeepsAtLeastOneActiveAdmin(t *testing.T) {
	store := openPlaybackTestStore(t)
	admin := createUserAdminTestActor(t, store, "sole-admin")

	_, err := store.UpdateUserAdmin(AdminUserUpdate{
		ActorID: admin.ID,
		UserID:  admin.ID,
		Role:    userAdminStringPtr(models.RoleUser),
	}, time.Now())
	if !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("sole admin demotion error = %v, want ErrLastActiveAdmin", err)
	}
	user, err := store.UserByID(admin.ID)
	if err != nil {
		t.Fatalf("read sole admin: %v", err)
	}
	if user.Role != models.RoleAdmin || user.Status != models.StatusActive {
		t.Errorf("sole admin changed after rejected demotion: %+v", user)
	}
}

func TestDeleteUserAdminRollsBackAndCascadesAtomically(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	actor := createUserAdminTestActor(t, store, "delete-actor")
	target := addAuthAtomicFixture(t, store, "delete-target", now)

	if _, err := store.DB.Exec(`CREATE TRIGGER injected_user_delete_failure
		BEFORE DELETE ON users
		BEGIN
			SELECT RAISE(ABORT, 'injected user delete failure');
		END`); err != nil {
		t.Fatalf("create delete failure trigger: %v", err)
	}
	if err := store.DeleteUserAdmin(
		actor.ID, target.playback.user.ID, now.Add(time.Minute),
	); err == nil {
		t.Fatal("delete unexpectedly succeeded despite injected failure")
	}
	if _, err := store.UserByID(target.playback.user.ID); err != nil {
		t.Fatalf("target disappeared after failed delete: %v", err)
	}
	assertAuthAtomicActive(t, store, target, now)

	if _, err := store.DB.Exec(`DROP TRIGGER injected_user_delete_failure`); err != nil {
		t.Fatalf("drop delete failure trigger: %v", err)
	}
	if err := store.DeleteUserAdmin(
		actor.ID, target.playback.user.ID, now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := store.UserByID(target.playback.user.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted user lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.SessionByToken(target.session); err == nil {
		t.Error("deleted user's Web session survived")
	}
	for _, check := range []struct {
		label string
		query string
	}{
		{"device", `SELECT COUNT(*) FROM devices WHERE user_id = ?`},
		{"device token", `SELECT COUNT(*) FROM device_access_tokens WHERE user_id = ?`},
		{"playback ticket", `SELECT COUNT(*) FROM playback_tickets WHERE user_id = ?`},
	} {
		var count int
		if err := store.DB.QueryRow(check.query, target.playback.user.ID).Scan(&count); err != nil {
			t.Fatalf("count %s after delete: %v", check.label, err)
		}
		if count != 0 {
			t.Errorf("%s rows after delete = %d, want 0", check.label, count)
		}
	}
}

func TestConcurrentAdminMutationsLeaveOneActiveAdmin(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*Store, int64, int64) error
	}{
		{
			name: "demote",
			mutate: func(store *Store, actorID, targetID int64) error {
				_, err := store.UpdateUserAdmin(AdminUserUpdate{
					ActorID: actorID,
					UserID:  targetID,
					Role:    userAdminStringPtr(models.RoleUser),
				}, now)
				return err
			},
		},
		{
			name: "disable",
			mutate: func(store *Store, actorID, targetID int64) error {
				_, err := store.UpdateUserAdmin(AdminUserUpdate{
					ActorID: actorID,
					UserID:  targetID,
					Status:  userAdminStringPtr(models.StatusDisabled),
				}, now)
				return err
			},
		},
		{
			name: "delete",
			mutate: func(store *Store, actorID, targetID int64) error {
				return store.DeleteUserAdmin(actorID, targetID, now)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first, second := openDeviceAuthConcurrencyStores(t)
			adminA := createUserAdminTestActor(t, first, tc.name+"-admin-a")
			adminB := createUserAdminTestActor(t, first, tc.name+"-admin-b")

			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				results <- tc.mutate(first, adminA.ID, adminB.ID)
			}()
			go func() {
				defer wg.Done()
				<-start
				results <- tc.mutate(second, adminB.ID, adminA.ID)
			}()
			close(start)
			wg.Wait()
			close(results)

			var successes int
			var failures []error
			for err := range results {
				if err == nil {
					successes++
				} else {
					failures = append(failures, err)
				}
			}
			if successes != 1 {
				t.Fatalf("successful mutations = %d, want exactly 1; failures: %v",
					successes, failures)
			}
			if len(failures) != 1 || !errors.Is(failures[0], ErrAdminActorUnavailable) {
				t.Fatalf("losing mutation errors = %v, want ErrAdminActorUnavailable", failures)
			}
			activeAdmins, err := first.ActiveAdminCount()
			if err != nil {
				t.Fatalf("count active admins: %v", err)
			}
			if activeAdmins != 1 {
				t.Fatalf("active admins after concurrent mutations = %d, want 1", activeAdmins)
			}
		})
	}
}
