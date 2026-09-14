package gitprovider_test

import (
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
)

// TestRefGuardMainPolicy: the platform-layer policy over main ref updates
// (T0302). Only the merge service may advance main; deletions are always
// refused; everything that is not main is unguarded.
func TestRefGuardMainPolicy(t *testing.T) {
	guard := gitprovider.RefGuard{MergeService: "post-git-svc"}
	cases := []struct {
		name    string
		update  gitprovider.RefUpdate
		wantErr error
	}{
		{
			name: "merge service advances main",
			update: gitprovider.RefUpdate{
				Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: "bbb", Actor: "post-git-svc",
			},
			wantErr: nil,
		},
		{
			name: "merge service advances main without refs prefix",
			update: gitprovider.RefUpdate{
				Ref: "main", OldSHA: "aaa", NewSHA: "bbb", Actor: "post-git-svc",
			},
			wantErr: nil,
		},
		{
			name: "human direct push to main",
			update: gitprovider.RefUpdate{
				Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: "bbb", Actor: "alice",
			},
			wantErr: gitprovider.ErrMainDirectPush,
		},
		{
			name: "admin direct push to main",
			update: gitprovider.RefUpdate{
				Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: "bbb", Actor: "postadmin",
			},
			wantErr: gitprovider.ErrMainDirectPush,
		},
		{
			name: "unknown actor creates main (first push)",
			update: gitprovider.RefUpdate{
				Ref: "refs/heads/main", OldSHA: "", NewSHA: "bbb", Actor: "alice",
			},
			wantErr: gitprovider.ErrMainDirectPush,
		},
		{
			name: "main deletion by the merge service",
			update: gitprovider.RefUpdate{
				Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: "0000000000000000000000000000000000000000", Actor: "post-git-svc",
			},
			wantErr: gitprovider.ErrMainDelete,
		},
		{
			name: "main deletion by a human",
			update: gitprovider.RefUpdate{
				Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: "0000000000000000000000000000000000000000", Actor: "alice",
			},
			wantErr: gitprovider.ErrMainDelete,
		},
		{
			name: "main deletion with empty new sha",
			update: gitprovider.RefUpdate{
				Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: "", Actor: "alice",
			},
			wantErr: gitprovider.ErrMainDelete,
		},
		{
			name: "research branch update by anyone",
			update: gitprovider.RefUpdate{
				Ref: "refs/heads/exp-42", OldSHA: "aaa", NewSHA: "bbb", Actor: "alice",
			},
			wantErr: nil,
		},
		{
			name: "research branch deletion by anyone",
			update: gitprovider.RefUpdate{
				Ref: "refs/heads/exp-42", OldSHA: "aaa", NewSHA: "0000000000000000000000000000000000000000", Actor: "alice",
			},
			wantErr: nil,
		},
		{
			name: "tag update",
			update: gitprovider.RefUpdate{
				Ref: "refs/tags/v1", OldSHA: "aaa", NewSHA: "bbb", Actor: "alice",
			},
			wantErr: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guard.Check(tc.update)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Check(%+v) = %v, want %v", tc.update, err, tc.wantErr)
			}
		})
	}
}

// TestRefGuardFailsClosed: an empty merge-service identity means no one —
// the merge service included — may advance main.
func TestRefGuardFailsClosed(t *testing.T) {
	guard := gitprovider.RefGuard{} // no merge service configured
	err := guard.Check(gitprovider.RefUpdate{
		Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: "bbb", Actor: "post-git-svc",
	})
	if !errors.Is(err, gitprovider.ErrMainDirectPush) {
		t.Fatalf("fail-closed guard = %v, want ErrMainDirectPush", err)
	}
}
