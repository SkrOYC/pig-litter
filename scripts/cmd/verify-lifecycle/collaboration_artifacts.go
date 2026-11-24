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

const (
	collaborationMaxArtifactBytes = 512 << 20
	collaborationMaxAttemptBytes  = 1 << 30
	collaborationMaxPNGBytes      = 64 << 20
)

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

type collaborationScreenshotSnapshot struct {
	artifact collaborationArtifact
	data     []byte
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

func collaborationReadArtifact(file *os.File, maxBytes int64, expected string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || int64(len(data)) > maxBytes {
		return nil, errors.New("artifact size is outside the permitted range")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expected {
		return nil, errors.New("artifact digest does not match")
	}
	return data, nil
}

func collaborationSnapshotVideo(source *os.File, expected string) (snapshot *os.File, resultErr error) {
	directory, err := os.MkdirTemp("", "p02-video-")
	if err != nil {
		return nil, err
	}
	name := filepath.Join(directory, "video")
	defer func() {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
		if resultErr != nil && snapshot != nil {
			_ = snapshot.Close()
			snapshot = nil
		}
	}()
	writer, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer writer.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(writer, hash), io.LimitReader(source, collaborationMaxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if size == 0 || size > collaborationMaxArtifactBytes || hex.EncodeToString(hash.Sum(nil)) != expected {
		return nil, errors.New("video digest or size is invalid")
	}
	writerInfo, err := writer.Stat()
	if err != nil {
		return nil, err
	}
	reader, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	readerInfo, err := reader.Stat()
	if err != nil || !os.SameFile(writerInfo, readerInfo) {
		reader.Close()
		return nil, errors.New("video snapshot was replaced")
	}
	current, err := os.Lstat(name)
	if err != nil || !os.SameFile(writerInfo, current) {
		reader.Close()
		return nil, errors.New("video snapshot path changed")
	}
	if err := os.Remove(name); err != nil {
		reader.Close()
		return nil, fmt.Errorf("detach video snapshot: %w", err)
	}
	if err := writer.Close(); err != nil {
		reader.Close()
		return nil, err
	}
	return reader, nil
}

func collaborationValidateArtifacts(ctx context.Context, root *os.Root, artifacts []collaborationArtifact, media collaborationMediaTools) (map[string]bool, bool, error) {
	if len(artifacts) > 128 {
		return nil, false, errors.New("too many artifacts")
	}
	kinds := make(map[string]bool)
	videos := make(map[string]*os.File)
	var screenshots []collaborationScreenshotSnapshot
	defer func() {
		for _, video := range videos {
			_ = video.Close()
		}
	}()
	paths := make(map[string]bool)
	resultsPassed := true
	var totalBytes int64
	for _, artifact := range artifacts {
		if paths[artifact.Path] {
			return nil, false, fmt.Errorf("duplicate artifact path %q", artifact.Path)
		}
		paths[artifact.Path] = true
		if err := collaborationValidateArtifactMetadata(artifact); err != nil {
			return nil, false, err
		}
		file, err := collaborationOpenFile(root, artifact.Path)
		if err != nil {
			return nil, false, err
		}
		switch artifact.Kind {
		case "screenshot":
			data, readErr := collaborationReadArtifact(file, collaborationMaxPNGBytes, artifact.SHA256)
			_ = file.Close()
			if readErr != nil {
				return nil, false, fmt.Errorf("screenshot %q: %w", artifact.Path, readErr)
			}
			if err := collaborationAddArtifactBytes(&totalBytes, int64(len(data))); err != nil {
				return nil, false, err
			}
			if err := collaborationValidatePNG(data); err != nil {
				return nil, false, fmt.Errorf("screenshot %q: %w", artifact.Path, err)
			}
			screenshots = append(screenshots, collaborationScreenshotSnapshot{artifact: artifact, data: data})
		case "video":
			snapshot, snapshotErr := collaborationSnapshotVideo(file, artifact.SHA256)
			_ = file.Close()
			if snapshotErr != nil {
				return nil, false, fmt.Errorf("video %q: %w", artifact.Path, snapshotErr)
			}
			info, err := snapshot.Stat()
			if err != nil {
				_ = snapshot.Close()
				return nil, false, err
			}
			if err := collaborationAddArtifactBytes(&totalBytes, info.Size()); err != nil {
				_ = snapshot.Close()
				return nil, false, err
			}
			if previous := videos[artifact.SHA256]; previous != nil {
				_ = previous.Close()
			}
			videos[artifact.SHA256] = snapshot
			if err := collaborationValidateVideo(ctx, snapshot, media); err != nil {
				return nil, false, fmt.Errorf("video %q: %w", artifact.Path, err)
			}
		case "result":
			data, readErr := collaborationReadArtifact(file, collaborationMaxJSONBytes, artifact.SHA256)
			_ = file.Close()
			if readErr != nil {
				return nil, false, fmt.Errorf("result %q: %w", artifact.Path, readErr)
			}
			if err := collaborationAddArtifactBytes(&totalBytes, int64(len(data))); err != nil {
				return nil, false, err
			}
			passed, err := collaborationResultPassed(data, artifact.Path)
			if err != nil {
				return nil, false, err
			}
			resultsPassed = resultsPassed && passed
		default:
			digest, size, digestErr := collaborationFileDigest(file, collaborationMaxArtifactBytes)
			_ = file.Close()
			if digestErr != nil || digest != artifact.SHA256 {
				return nil, false, fmt.Errorf("artifact %q has an invalid digest or size", artifact.Path)
			}
			if err := collaborationAddArtifactBytes(&totalBytes, size); err != nil {
				return nil, false, err
			}
		}
		kinds[artifact.Kind] = true
	}
	for _, screenshot := range screenshots {
		artifact := screenshot.artifact
		if artifact.Origin != "video_frame" {
			continue
		}
		video := videos[artifact.SourceVideo]
		if video == nil || artifact.SourceTimestamp == nil || *artifact.SourceTimestamp < 0 {
			return nil, false, fmt.Errorf("screenshot %q lacks its source video and timestamp", artifact.Path)
		}
		if err := collaborationCompareVideoFrame(ctx, screenshot, video, media); err != nil {
			return nil, false, err
		}
	}
	return kinds, kinds["result"] && resultsPassed, nil
}

func collaborationAddArtifactBytes(total *int64, size int64) error {
	if size < 0 || size > collaborationMaxAttemptBytes-*total {
		return errors.New("attempt artifacts exceed the size limit")
	}
	*total += size
	return nil
}

func collaborationValidateArtifactMetadata(artifact collaborationArtifact) error {
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
	return nil
}

func collaborationResultPassed(data []byte, path string) (bool, error) {
	var values map[string]json.RawMessage
	if err := collaborationDecodeJSON(data, &values); err != nil || values == nil {
		return false, fmt.Errorf("result %q is not a JSON object", path)
	}
	var found, result bool
	for _, name := range []string{"ok", "passed"} {
		raw, exists := values[name]
		if !exists {
			continue
		}
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil || (found && value != result) {
			return false, fmt.Errorf("result %q has invalid or conflicting status", path)
		}
		found, result = true, value
	}
	if !found {
		return false, fmt.Errorf("result %q has no status", path)
	}
	return result, nil
}

func collaborationValidatePNG(data []byte) error {
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 10000 || config.Height > 10000 || int64(config.Width)*int64(config.Height) > 16_000_000 {
		return errors.New("invalid PNG dimensions or header")
	}
	_, err = png.Decode(bytes.NewReader(data))
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

func collaborationMediaInput() []string {
	return []string{"-protocol_whitelist", "fd", "-f", "mov", "-enable_drefs", "0", "-use_absolute_path", "0", "-max_pixels", "16000000", "-threads", "1", "-fd", "0", "-i", "fd:"}
}

func collaborationValidateVideo(ctx context.Context, file *os.File, media collaborationMediaTools) error {
	if media.FFprobe == "" || media.FFmpeg == "" {
		return errors.New("video validation requires ffprobe and ffmpeg")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	probeArgs := []string{"-v", "error", "-max_alloc", "268435456", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=width,height,nb_read_frames:format=duration", "-of", "json"}
	probeArgs = append(probeArgs, collaborationMediaInput()...)
	probe, err := collaborationMediaCommand(ctx, file, media.FFprobe, 64<<10, probeArgs...)
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
	if frameErr != nil || durationErr != nil || frames == 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 24*60*60 || stream.Width <= 0 || stream.Height <= 0 || stream.Width > 10000 || stream.Height > 10000 || int64(stream.Width)*int64(stream.Height) > 16_000_000 {
		return errors.New("video has invalid frame count, duration, or dimensions")
	}
	decodeArgs := []string{"-v", "error", "-max_alloc", "268435456", "-filter_threads", "1", "-xerror", "-err_detect", "explode"}
	decodeArgs = append(decodeArgs, collaborationMediaInput()...)
	decodeArgs = append(decodeArgs, "-map", "0:v:0", "-f", "null", "-")
	_, err = collaborationMediaCommand(ctx, file, media.FFmpeg, 64<<10, decodeArgs...)
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
	shellArgs := append([]string{"-c", `ulimit -v 2097152 && ulimit -t 90 && exec "$@"`, "p02-media", binary}, args...)
	command := exec.CommandContext(ctx, "/bin/sh", shellArgs...)
	command.Stdin = file
	output := collaborationBoundedOutput{limit: outputLimit}
	diagnostics := collaborationBoundedOutput{limit: 64 << 10}
	command.Stdout, command.Stderr = &output, &diagnostics
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", binary, err, strings.TrimSpace(diagnostics.String()))
	}
	return output.Bytes(), nil
}

func collaborationCompareVideoFrame(ctx context.Context, screenshot collaborationScreenshotSnapshot, video *os.File, media collaborationMediaTools) error {
	stamp := strconv.FormatFloat(float64(*screenshot.artifact.SourceTimestamp)/1000, 'f', 3, 64)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := []string{"-v", "error", "-max_alloc", "268435456", "-filter_threads", "1", "-xerror", "-ss", stamp}
	args = append(args, collaborationMediaInput()...)
	args = append(args, "-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "-threads:v", "1", "pipe:1")
	frame, err := collaborationMediaCommand(ctx, video, media.FFmpeg, 32<<20, args...)
	if err != nil {
		return fmt.Errorf("extract screenshot source frame: %w", err)
	}
	actual, err := png.Decode(bytes.NewReader(frame))
	if err != nil {
		return fmt.Errorf("decode source frame: %w", err)
	}
	expected, err := png.Decode(bytes.NewReader(screenshot.data))
	if err != nil {
		return fmt.Errorf("decode screenshot: %w", err)
	}
	if !collaborationImagesEqual(actual, expected) {
		return fmt.Errorf("screenshot %q differs from the selected video frame", screenshot.artifact.Path)
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
