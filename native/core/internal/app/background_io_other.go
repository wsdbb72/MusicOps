//go:build !windows

package app

func runWithBackgroundIO(fn func() error) error {
	return fn()
}
