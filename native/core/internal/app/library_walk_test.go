package app

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

func TestSkipLibraryWalkError(t *testing.T) {
	const root = "/music"
	tests := []struct {
		name string
		path string
		err  error
		want bool
	}{
		{"permission on optional child", "/music/.thumbnails", fs.ErrPermission, true},
		{"child vanishes during walk", "/music/vanished.flac", fs.ErrNotExist, true},
		{"path error on optional directory", "/music/.thumbnails", &os.PathError{Op: "readdir", Path: "/music/.thumbnails", Err: fs.ErrPermission}, true},
		{"root permission remains fatal", root, fs.ErrPermission, false},
		{"persistent child I/O error remains fatal", "/music/track.flac", errors.New("input/output error"), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := skipLibraryWalkError(root, test.path, test.err); got != test.want {
				t.Fatalf("skipLibraryWalkError() = %v, want %v", got, test.want)
			}
		})
	}
}
