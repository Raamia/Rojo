package jobs

import (
	"errors"
	"strings"
	"testing"
)

func TestNewJobRequest_Validate(t *testing.T) {
	cases := []struct {
		name    string
		req     NewJobRequest
		wantErr error
	}{
		{
			name:    "valid request",
			req:     NewJobRequest{Task: "add a new endpoint for listing users", RepoPath: "/tmp/repo"},
			wantErr: nil,
		},
		{
			name:    "empty task",
			req:     NewJobRequest{Task: "", RepoPath: "/tmp/repo"},
			wantErr: ErrTaskRequired,
		},
		{
			name:    "whitespace-only task",
			req:     NewJobRequest{Task: "   \t\n", RepoPath: "/tmp/repo"},
			wantErr: ErrTaskRequired,
		},
		{
			name:    "task too short",
			req:     NewJobRequest{Task: "hi", RepoPath: "/tmp/repo"},
			wantErr: ErrTaskTooShort,
		},
		{
			name:    "task too long",
			req:     NewJobRequest{Task: strings.Repeat("a", maxTaskLength+1), RepoPath: "/tmp/repo"},
			wantErr: ErrTaskTooLong,
		},
		{
			name:    "missing repo path",
			req:     NewJobRequest{Task: "do the thing", RepoPath: ""},
			wantErr: ErrRepoPathMissing,
		},
		{
			name:    "whitespace-only repo path",
			req:     NewJobRequest{Task: "do the thing", RepoPath: "   "},
			wantErr: ErrRepoPathMissing,
		},
		{
			name:    "relative repo path",
			req:     NewJobRequest{Task: "do the thing", RepoPath: "./repo"},
			wantErr: ErrRepoPathRelative,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got err=%v, want %v", err, tc.wantErr)
			}
		})
	}
}
