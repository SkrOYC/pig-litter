package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const collaborationMaxArtifactBytes = 512 << 20

type collaborationArtifact struct {
	Path            string `json:"path"`
	SHA256          string `json:"sha256"`
	Kind            string `json:"kind"`
	Origin          string `json:"origin,omitempty"`
	SourceVideo     string `json:"source_video_sha256,omitempty"`
	SourceTimestamp *int64 `json:"source_timestamp_ms,omitempty"`
}

type collaborationMediaTools struct {
	FFprobe string
	FFmpeg  string
}

func collaborationOpenFile(root *os.Root, name string) (*os.File, error) {
	if !filepath.IsLocal(name) || filepath.Clean(name) != name || name == "." {
		return nil, fmt.Errorf("invalid relative path %q", name)
	}
	parts := strings.Split(name, string(filepath.Separator))
	var final os.FileInfo
	for index := range parts {
		part := filepath.Join(parts[:index+1]...)
		info, err := root.Lstat(part)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symbolic link in %q", name)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("non-directory in %q", name)
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("not a regular file: %q", name)
		}
		if index == len(parts)-1 {
			final = info
		}
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(final, info) {
		file.Close()
		return nil, fmt.Errorf("artifact is not a regular file: %q", name)
	}
	return file, nil
}

func collaborationFileDigest(file *os.File, maxBytes int64) (string, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maxBytes+1))
	if err != nil {
		return "", 0, err
	}
	if size == 0 || size > maxBytes {
		return "", size, fmt.Errorf("file size %d is outside the permitted range", size)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func collaborationValidateArtifacts(ctx context.Context, root *os.Root, artifacts []collaborationArtifact, media collaborationMediaTools) (map[string]bool, bool, error) {
	kinds := make(map[string]bool)
	videos := make(map[string]collaborationArtifact)
	paths := make(map[string]bool)
	resultsPassed := true
	for _, artifact := range artifacts {
		if paths[artifact.Path] {
			return nil, false, fmt.Errorf("duplicate artifact path %q", artifact.Path)
		}
		paths[artifact.Path] = true
		if err := collaborationValidateArtifact(ctx, root, artifact, media); err != nil {
			return nil, false, err
		}
		if artifact.Kind == "result" {
			passed, err := collaborationResultPassed(root, artifact)
			if err != nil {
				return nil, false, err
			}
			resultsPassed = resultsPassed && passed
		}
		kinds[artifact.Kind] = true
		if artifact.Kind == "video" {
			videos[artifact.SHA256] = artifact
		}
	}
	for _, artifact := range artifacts {
		if artifact.Kind != "screenshot" || artifact.Origin != "video_frame" {
			continue
		}
		video, exists := videos[artifact.SourceVideo]
		if !exists || artifact.SourceTimestamp == nil || *artifact.SourceTimestamp < 0 {
			return nil, false, fmt.Errorf("screenshot %q lacks its source video and timestamp", artifact.Path)
		}
		if err := collaborationCompareVideoFrame(ctx, root, artifact, video, media); err != nil {
			return nil, false, err
		}
	}
	return kinds, kinds["result"] && resultsPassed, nil
}

func collaborationResultPassed(root *os.Root, artifact collaborationArtifact) (bool, error) {
	data, err := collaborationReadSelectedFile(root, artifact.Path, artifact.SHA256)
	if err != nil {
		return false, fmt.Errorf("result %q: %w", artifact.Path, err)
	}
	var values map[string]json.RawMessage
	if err := collaborationDecodeJSON(data, &values); err != nil || values == nil {
		return false, fmt.Errorf("result %q is not a JSON object", artifact.Path)
	}
	var found, result bool
	for _, name := range []string{"ok", "passed"} {
		raw, exists := values[name]
		if !exists {
			continue
		}
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil || (found && value != result) {
			return false, fmt.Errorf("result %q has invalid or conflicting status", artifact.Path)
		}
		found, result = true, value
	}
	if !found {
		return false, fmt.Errorf("result %q has no status", artifact.Path)
	}
	return result, nil
}

func collaborationValidateArtifact(ctx context.Context, root *os.Root, artifact collaborationArtifact, media collaborationMediaTools) error {
	if !collaborationOneOf(artifact.Kind, "log", "ansi", "pty", "timing", "result", "samples", "screenshot", "video") || !collaborationSHA256(artifact.SHA256) {
		return fmt.Errorf("invalid artifact metadata for %q", artifact.Path)
	}
	if artifact.Kind == "video" && artifact.Origin != "live_terminal" {
		return fmt.Errorf("media %q is not attributed to a live terminal", artifact.Path)
	}
	if artifact.Kind == "screenshot" && !collaborationOneOf(artifact.Origin, "live_terminal", "video_frame") {
		return fmt.Errorf("screenshot %q is not attributed to a live terminal", artifact.Path)
	}
	if artifact.Kind != "screenshot" && artifact.Kind != "video" && artifact.Origin != "" {
		return fmt.Errorf("artifact %q has unexpected media origin", artifact.Path)
	}
	if artifact.Origin != "video_frame" && (artifact.SourceVideo != "" || artifact.SourceTimestamp != nil) {
		return fmt.Errorf("artifact %q has unexpected video-frame metadata", artifact.Path)
	}
	file, err := collaborationOpenFile(root, artifact.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	digest, _, err := collaborationFileDigest(file, collaborationMaxArtifactBytes)
	if err != nil || digest != artifact.SHA256 {
		return fmt.Errorf("artifact %q has an invalid digest or size", artifact.Path)
	}
	switch artifact.Kind {
	case "screenshot":
		if err := collaborationValidatePNG(file); err != nil {
			return fmt.Errorf("screenshot %q: %w", artifact.Path, err)
		}
	case "video":
		if err := collaborationValidateVideo(ctx, file, media); err != nil {
			return fmt.Errorf("video %q: %w", artifact.Path, err)
		}
	}
	after, _, err := collaborationFileDigest(file, collaborationMaxArtifactBytes)
	if err != nil || after != digest {
		return fmt.Errorf("artifact %q changed during validation", artifact.Path)
	}
	return nil
}

func collaborationValidatePNG(file *os.File) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	config, err := png.DecodeConfig(file)
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 10000 || config.Height > 10000 || int64(config.Width)*int64(config.Height) > 16_000_000 {
		return errors.New("invalid PNG dimensions or header")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = png.Decode(file)
	return err
}

type collaborationProbeOutput struct {
	Streams []struct {
		Width  int    `json:"width"`
		Height int    `json:"height"`
		Frames string `json:"nb_read_frames"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func collaborationValidateVideo(ctx context.Context, file *os.File, media collaborationMediaTools) error {
	if media.FFprobe == "" || media.FFmpeg == "" {
		return errors.New("video validation requires ffprobe and ffmpeg")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	probe, err := collaborationMediaCommand(ctx, file, media.FFprobe, 64<<10, "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=width,height,nb_read_frames:format=duration", "-of", "json", "-i", "/dev/stdin")
	if err != nil {
		return fmt.Errorf("probe failed: %w", err)
	}
	var output collaborationProbeOutput
	if err := json.Unmarshal(probe, &output); err != nil || len(output.Streams) != 1 {
		return errors.New("invalid video probe output")
	}
	stream := output.Streams[0]
	frames, frameErr := strconv.ParseUint(stream.Frames, 10, 64)
	duration, durationErr := strconv.ParseFloat(output.Format.Duration, 64)
	if frameErr != nil || durationErr != nil || frames == 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 24*60*60 || stream.Width <= 0 || stream.Height <= 0 || stream.Width > 10000 || stream.Height > 10000 {
		return errors.New("video has invalid frame count, duration, or dimensions")
	}
	_, err = collaborationMediaCommand(ctx, file, media.FFmpeg, 64<<10, "-v", "error", "-xerror", "-err_detect", "explode", "-i", "/dev/stdin", "-map", "0:v:0", "-f", "null", "-")
	if err != nil {
		return fmt.Errorf("video decode failed: %w", err)
	}
	return nil
}

type collaborationBoundedOutput struct {
	bytes.Buffer
	limit int
}

func (buffer *collaborationBoundedOutput) Write(data []byte) (int, error) {
	if buffer.Len()+len(data) > buffer.limit {
		return 0, errors.New("media tool output exceeds limit")
	}
	return buffer.Buffer.Write(data)
}

func collaborationMediaCommand(ctx context.Context, file *os.File, binary string, outputLimit int, args ...string) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Stdin = file
	output := collaborationBoundedOutput{limit: outputLimit}
	diagnostics := collaborationBoundedOutput{limit: 64 << 10}
	command.Stdout, command.Stderr = &output, &diagnostics
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", binary, err, strings.TrimSpace(diagnostics.String()))
	}
	return output.Bytes(), nil
}

func collaborationCompareVideoFrame(ctx context.Context, root *os.Root, screenshot, video collaborationArtifact, media collaborationMediaTools) error {
	videoFile, err := collaborationOpenFile(root, video.Path)
	if err != nil {
		return err
	}
	defer videoFile.Close()
	imageFile, err := collaborationOpenFile(root, screenshot.Path)
	if err != nil {
		return err
	}
	defer imageFile.Close()
	imageDigest, _, err := collaborationFileDigest(imageFile, collaborationMaxArtifactBytes)
	if err != nil || imageDigest != screenshot.SHA256 {
		return fmt.Errorf("screenshot %q changed", screenshot.Path)
	}
	before, _, err := collaborationFileDigest(videoFile, collaborationMaxArtifactBytes)
	if err != nil || before != video.SHA256 {
		return fmt.Errorf("source video %q changed", video.Path)
	}
	stamp := strconv.FormatFloat(float64(*screenshot.SourceTimestamp)/1000, 'f', 3, 64)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	frame, err := collaborationMediaCommand(ctx, videoFile, media.FFmpeg, 32<<20, "-v", "error", "-xerror", "-ss", stamp, "-i", "/dev/stdin", "-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1")
	if err != nil {
		return fmt.Errorf("extract screenshot source frame: %w", err)
	}
	actual, err := png.Decode(bytes.NewReader(frame))
	if err != nil {
		return fmt.Errorf("decode source frame: %w", err)
	}
	if _, err := imageFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	expected, err := png.Decode(imageFile)
	if err != nil {
		return fmt.Errorf("decode screenshot: %w", err)
	}
	if !collaborationImagesEqual(actual, expected) {
		return fmt.Errorf("screenshot %q differs from the selected video frame", screenshot.Path)
	}
	after, _, err := collaborationFileDigest(videoFile, collaborationMaxArtifactBytes)
	if err != nil || after != before {
		return fmt.Errorf("source video %q changed during frame comparison", video.Path)
	}
	afterImage, _, err := collaborationFileDigest(imageFile, collaborationMaxArtifactBytes)
	if err != nil || afterImage != imageDigest {
		return fmt.Errorf("screenshot %q changed during frame comparison", screenshot.Path)
	}
	return nil
}

func collaborationImagesEqual(left, right image.Image) bool {
	if left.Bounds() != right.Bounds() {
		return false
	}
	for y := left.Bounds().Min.Y; y < left.Bounds().Max.Y; y++ {
		for x := left.Bounds().Min.X; x < left.Bounds().Max.X; x++ {
			lr, lg, lb, la := left.At(x, y).RGBA()
			rr, rg, rb, ra := right.At(x, y).RGBA()
			if lr != rr || lg != rg || lb != rb || la != ra {
				return false
			}
		}
	}
	return true
}

func collaborationSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
