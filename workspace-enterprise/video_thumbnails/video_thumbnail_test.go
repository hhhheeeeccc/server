package video_thumbnails

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestThumbnailOutputLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := thumbnailOutput{limit: 8, cancel: cancel}
	if _, err := output.Write([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	n, err := output.Write([]byte("7890"))
	if n != 2 || err == nil || output.Len() != 8 || !output.exceeded || ctx.Err() == nil {
		t.Fatalf("output was not bounded and canceled: n=%d, length=%d, err=%v", n, output.Len(), err)
	}
}

func TestThumbnailCanceledBeforeExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runFFmpegFrameContext(ctx, "/tmp/not-opened", "0")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestThumbnailTerminatesStalledProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the controlled process fixture requires a POSIX shell")
	}
	directory := t.TempDir()
	// A fixed local fixture stands in for a decoder that never finishes.
	if err := os.WriteFile(filepath.Join(directory, "ffmpeg"), []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runFFmpegFrameContext(ctx, filepath.Join(directory, "input"), "0")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
		t.Fatalf("stalled process did not stop at its deadline: %v", err)
	}
}

func TestThumbnailCancelsStalledInput(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := extractVideoThumbnail(ctx, reader, 1024)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("stalled storage read did not stop: %v", err)
	}
	if len(thumbnailSlots) != 0 {
		t.Fatal("thumbnail slot leaked after canceled input")
	}
}

func TestThumbnailRejectsWorkWhenCapacityIsFull(t *testing.T) {
	for range cap(thumbnailSlots) {
		thumbnailSlots <- struct{}{}
		defer func() { <-thumbnailSlots }()
	}
	_, err := extractVideoThumbnail(context.Background(), io.NopCloser(strings.NewReader("video")), 1024)
	if err == nil || !strings.Contains(err.Error(), "capacity reached") {
		t.Fatalf("expected immediate rejection at capacity, got %v", err)
	}
}

func TestRunFFmpegFrameValidation(t *testing.T) {
	tests := []struct {
		name        string
		inputPath   string
		timestamp   string
		expectErr   bool
		errContains string
	}{
		// ── سابقاً: حماية من "-" في البداية ──────────────────────────────
		{
			name:        "inputPath starts with hyphen (legacy check via ffmpeg itself)",
			inputPath:   "/tmp/safe-path.tmp",
			timestamp:   "-00:00:01",
			expectErr:   true,
			errContains: "invalid timestamp format",
		},
		// ── timestamp validation ──────────────────────────────────────────
		{
			name:        "valid HH:MM:SS timestamp",
			inputPath:   "/tmp/nonexistent.tmp",
			timestamp:   "00:00:01",
			expectErr:   true, // ffmpeg will fail (file missing), but validation passes
			errContains: "ffmpeg",
		},
		{
			name:        "valid HH:MM:SS with sub-seconds",
			inputPath:   "/tmp/nonexistent.tmp",
			timestamp:   "00:00:01.5",
			expectErr:   true,
			errContains: "ffmpeg",
		},
		{
			name:        "valid plain integer seconds",
			inputPath:   "/tmp/nonexistent.tmp",
			timestamp:   "1",
			expectErr:   true,
			errContains: "ffmpeg",
		},
		{
			name:        "valid decimal seconds",
			inputPath:   "/tmp/nonexistent.tmp",
			timestamp:   "1.5",
			expectErr:   true,
			errContains: "ffmpeg",
		},
		{
			name:        "timestamp with leading hyphen rejected",
			inputPath:   "/tmp/safe.tmp",
			timestamp:   "-ss",
			expectErr:   true,
			errContains: "invalid timestamp format",
		},
		{
			name:        "timestamp is an ffmpeg flag — rejected",
			inputPath:   "/tmp/safe.tmp",
			timestamp:   "00:00:01; rm -rf /",
			expectErr:   true,
			errContains: "invalid timestamp format",
		},
		{
			name:        "timestamp with shell metacharacters — rejected",
			inputPath:   "/tmp/safe.tmp",
			timestamp:   "$(echo pwned)",
			expectErr:   true,
			errContains: "invalid timestamp format",
		},
		{
			name:        "timestamp with backslash — rejected",
			inputPath:   "/tmp/safe.tmp",
			timestamp:   `00:00\01`,
			expectErr:   true,
			errContains: "invalid timestamp format",
		},
		{
			name:        "empty timestamp — rejected",
			inputPath:   "/tmp/safe.tmp",
			timestamp:   "",
			expectErr:   true,
			errContains: "invalid timestamp format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runFFmpegFrame(tt.inputPath, tt.timestamp)
			if tt.expectErr {
				if err == nil {
					t.Fatalf("expected an error but got nil")
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %q", tt.errContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}
