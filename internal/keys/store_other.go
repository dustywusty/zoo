//go:build !windows

package keys

func platformStore() store { return nil }
