package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func collaborationWriteArtifact(t *testing.T, dir, name, kind, origin string, data []byte) collaborationArtifact {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return collaborationArtifact{Path: name, Kind: kind, Origin: origin, SHA256: collaborationTestDigest(data)}
}

func collaborationPNG(t *testing.T, pixel color.RGBA) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 16, 16))
	picture.SetRGBA(0, 0, pixel)
	var output bytes.Buffer
	if err := png.Encode(&output, picture); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestCollaborationChecksPNGAndResultContents(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	pngData := collaborationPNG(t, color.RGBA{R: 255, A: 255})
	screenshot := collaborationWriteArtifact(t, dir, "screen.png", "screenshot", "live_terminal", pngData)
	result := collaborationWriteArtifact(t, dir, "result.json", "result", "", []byte(`{"ok":true}`))
	kinds, passed, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{screenshot, result}, collaborationMediaTools{})
	if err != nil || !kinds["screenshot"] || !kinds["result"] || !passed {
		t.Fatalf("valid artifacts = %#v, %v, %v", kinds, passed, err)
	}
	broken := collaborationWriteArtifact(t, dir, "broken.png", "screenshot", "live_terminal", pngData[:len(pngData)/2])
	if _, _, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{broken}, collaborationMediaTools{}); err == nil {
		t.Fatal("accepted a truncated PNG with a matching digest")
	}
	falseResult := collaborationWriteArtifact(t, dir, "failed.json", "result", "", []byte(`{"passed":false}`))
	_, passed, err = collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{falseResult}, collaborationMediaTools{})
	if err != nil || passed {
		t.Fatalf("failed result = %v, %v", passed, err)
	}
	contradictory := collaborationWriteArtifact(t, dir, "conflict.json", "result", "", []byte(`{"ok":true,"passed":false}`))
	if _, _, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{contradictory}, collaborationMediaTools{}); err == nil {
		t.Fatal("accepted contradictory result fields")
	}
}

func TestCollaborationRejectsArtifactEscapeAndSyntheticMedia(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, _, err := collaborationValidateArtifacts(context.Background(), root, make([]collaborationArtifact, 129), collaborationMediaTools{}); err == nil || !strings.Contains(err.Error(), "too many artifacts") {
		t.Fatalf("artifact-count limit error = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	data := collaborationPNG(t, color.RGBA{B: 255, A: 255})
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.png")); err != nil {
		t.Fatal(err)
	}
	link := collaborationArtifact{Path: "link.png", Kind: "screenshot", Origin: "live_terminal", SHA256: collaborationTestDigest(data)}
	if _, _, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{link}, collaborationMediaTools{}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink error = %v", err)
	}
	local := collaborationWriteArtifact(t, dir, "synthetic.png", "screenshot", "reconstructed", data)
	if _, _, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{local}, collaborationMediaTools{}); err == nil || !strings.Contains(err.Error(), "live terminal") {
		t.Fatalf("synthetic image error = %v", err)
	}
	local.Origin = "video_frame"
	if _, _, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{local}, collaborationMediaTools{}); err == nil || !strings.Contains(err.Error(), "source video") {
		t.Fatalf("unbound video frame error = %v", err)
	}
}

func TestCollaborationVideoSnapshotKeepsSelectedBytes(t *testing.T) {
	name := filepath.Join(t.TempDir(), "source.mp4")
	original := []byte("selected bytes")
	if err := os.WriteFile(name, original, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshot, err := collaborationSnapshotVideo(source, collaborationTestDigest(original))
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if err := os.WriteFile(name, []byte("changed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(snapshot)
	if err != nil || string(data) != "selected bytes" {
		t.Fatalf("immutable video snapshot = %q, %v", data, err)
	}
}

func TestCollaborationChecksVideoAndFrameContents(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ffmpeg := os.Getenv("COLLAB_TEST_FFMPEG")
	ffprobe := os.Getenv("COLLAB_TEST_FFPROBE")
	if ffmpeg == "" || ffprobe == "" {
		invalid := collaborationWriteArtifact(t, dir, "video.mp4", "video", "live_terminal", []byte("not a video"))
		_, _, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{invalid}, collaborationMediaTools{})
		if err == nil || !strings.Contains(err.Error(), "requires ffprobe and ffmpeg") {
			t.Fatalf("missing tools error = %v", err)
		}
		t.Log("Real-video validation requires explicit COLLAB_TEST_FFMPEG and COLLAB_TEST_FFPROBE paths.")
		return
	}
	videoPath := filepath.Join(dir, "video.mp4")
	command := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=32x32:r=2:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", videoPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create video fixture: %v: %s", err, output)
	}
	videoData, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	video := collaborationArtifact{Path: "video.mp4", Kind: "video", Origin: "live_terminal", SHA256: collaborationTestDigest(videoData)}
	imagePath := filepath.Join(dir, "frame.png")
	command = exec.Command(ffmpeg, "-v", "error", "-ss", "0.500", "-i", videoPath, "-frames:v", "1", imagePath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("extract video fixture frame: %v: %s", err, output)
	}
	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	stamp := int64(500)
	frame := collaborationArtifact{Path: "frame.png", Kind: "screenshot", Origin: "video_frame", SHA256: collaborationTestDigest(imageData), SourceVideo: video.SHA256, SourceTimestamp: &stamp}
	tools := collaborationMediaTools{FFmpeg: ffmpeg, FFprobe: ffprobe}
	kinds, passed, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{video, frame}, tools)
	if err != nil || !kinds["video"] || !kinds["screenshot"] || passed {
		t.Fatalf("valid video artifacts = %#v, %v, %v", kinds, passed, err)
	}
	other := collaborationWriteArtifact(t, dir, "different.png", "screenshot", "video_frame", collaborationPNG(t, color.RGBA{R: 255, A: 255}))
	other.SourceVideo, other.SourceTimestamp = video.SHA256, &stamp
	if _, _, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{video, other}, tools); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("incorrect frame error = %v", err)
	}
	broken := collaborationWriteArtifact(t, dir, "broken.mp4", "video", "live_terminal", videoData[:len(videoData)/2])
	if _, _, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{broken}, tools); err == nil {
		t.Fatal("accepted a truncated video with a matching digest")
	}
	playlist := collaborationWriteArtifact(t, dir, "playlist.m3u8", "video", "live_terminal", []byte("#EXTM3U\n#EXT-X-ENDLIST\n"))
	if _, _, err := collaborationValidateArtifacts(context.Background(), root, []collaborationArtifact{playlist}, tools); err == nil {
		t.Fatal("accepted a playlist as a self-contained terminal video")
	}
}
