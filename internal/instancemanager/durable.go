/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package instancemanager

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// replaceFile writes data to name below root, readable by its owner only, so
// that a crash at any moment leaves either the earlier content of name or
// data, never a truncated or partial file, and data survives a power loss
// once replaceFile returned.
// It writes a temporary file beside name, syncs it, renames it over name and
// syncs the directory. The temporary file has a fixed name, so one left by a
// crash is reused by the next write instead of piling up; writers of the
// same name are serialized by their callers.
func replaceFile(root *os.Root, name string, data []byte) error {
	tmp := name + ".im-tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = root.Rename(tmp, name)
	}
	if err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return syncDir(root, filepath.Dir(name))
}

// removeFile removes name below root, if it exists, and syncs its directory,
// so that the removal survives a power loss.
func removeFile(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return syncDir(root, filepath.Dir(name))
}

// syncDirPath is syncDir for a directory named by its path.
func syncDirPath(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return syncDir(root, ".")
}

// syncDir makes the entries of dir below root durable: a file created,
// renamed or removed there, or a directory made there.
func syncDir(root *os.Root, dir string) error {
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}
