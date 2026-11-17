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

package evidence

import (
	"regexp"
	"strings"
)

const repositoryComponent = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`

var (
	imageDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
	// A registry can include a numeric port. Repository components cannot
	// contain credentials, a URL scheme or an image tag.
	imageRepository = regexp.MustCompile(`^(?:[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]+)?/)?` +
		repositoryComponent + `(?:/` + repositoryComponent + `)*$`)
)

// ValidDigestImage reports whether image names a repository pinned by SHA-256.
func ValidDigestImage(image string) bool {
	repository, digest, found := strings.Cut(image, "@sha256:")
	return found && imageDigest.MatchString(digest) && imageRepository.MatchString(repository)
}

// ValidPinnedImage also accepts the development tag for the selected commit.
func ValidPinnedImage(image, commit string) bool {
	if strings.Contains(image, "@sha256:") {
		return ValidDigestImage(image)
	}
	repository, found := strings.CutSuffix(image, ":main-"+commit)
	return found && imageRepository.MatchString(repository)
}
