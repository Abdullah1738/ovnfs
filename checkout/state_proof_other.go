//go:build !darwin

package checkout

import "os"

func lockProof(string) (*os.File, error) { return nil, nil }
func proofOwner(string) (int, error)     { return 0, nil }
