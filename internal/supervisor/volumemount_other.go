//go:build !linux

package supervisor

// openAndIsolateVolume rejects volume registration on non-Linux
// hosts. The bind-mount + MS_PRIVATE mechanism is Linux-specific;
// silent no-op would let a tenant Postgres data dir live in the
// macOS dev host's tmpfs and lose data on next restart.
func (s *Supervisor) openAndIsolateVolume(_ string) (*Volume, string, error) {
	return nil, "", ErrVolumeMountUnsupported
}

// ensureVolumeRootFD on non-Linux always errors. Pure-validation
// callers never reach here.
func (s *Supervisor) ensureVolumeRootFD() (int, error) {
	return -1, ErrVolumeMountUnsupported
}

// applyVolumeMounts on non-Linux: validation runs (so devs see
// consistent error messages when authoring configs on macOS), but
// any non-empty VolumeMounts returns ErrVolumeMountUnsupported.
func (s *Supervisor) applyVolumeMounts(mounts []VolumeMount, chrootDir string) error {
	resolved, err := s.resolveVolumeMounts(mounts, chrootDir)
	if err != nil {
		return err
	}
	if len(resolved) == 0 {
		return nil
	}
	return ErrVolumeMountUnsupported
}
