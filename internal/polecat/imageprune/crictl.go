package imageprune

import (
	"encoding/json"
	"fmt"
	"time"
)

// ParseImages reads `crictl images -o json`. Creation times are not in that
// output; ParseCreated reads them from `crictl inspecti -o json`.
func ParseImages(data []byte) ([]Image, error) {
	var out struct {
		Images []struct {
			ID          string   `json:"id"`
			RepoTags    []string `json:"repoTags"`
			RepoDigests []string `json:"repoDigests"`
		} `json:"images"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("crictl images: %w", err)
	}
	images := make([]Image, 0, len(out.Images))
	for _, im := range out.Images {
		images = append(images, Image{ID: im.ID, Tags: im.RepoTags, Digests: im.RepoDigests})
	}
	return images, nil
}

// ParseCreated reads an image's creation time from `crictl inspecti -o json`
// (info.imageSpec.created).
func ParseCreated(data []byte) (time.Time, error) {
	var out struct {
		Info struct {
			ImageSpec struct {
				Created string `json:"created"`
			} `json:"imageSpec"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return time.Time{}, fmt.Errorf("crictl inspecti: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, out.Info.ImageSpec.Created)
	if err != nil {
		return time.Time{}, fmt.Errorf("crictl inspecti: created %q: %w", out.Info.ImageSpec.Created, err)
	}
	return t, nil
}

// InUse returns the image references and ids of the containers in
// `crictl ps -a -o json`: an image a container uses is never a candidate.
func InUse(data []byte) (map[string]bool, error) {
	var out struct {
		Containers []struct {
			Image struct {
				Image string `json:"image"`
			} `json:"image"`
			ImageRef string `json:"imageRef"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("crictl ps: %w", err)
	}
	used := map[string]bool{}
	for _, c := range out.Containers {
		if c.Image.Image != "" {
			used[c.Image.Image] = true
		}
		if c.ImageRef != "" {
			used[c.ImageRef] = true
		}
	}
	return used, nil
}
