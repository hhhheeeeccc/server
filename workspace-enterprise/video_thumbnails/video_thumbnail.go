// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package video_thumbnails

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"time"
)

const thumbnailTimeout = 30 * time.Second

var thumbnailSlots = make(chan struct{}, 2)

// validTimestamp matches HH:MM:SS or a plain non-negative number (e.g. "1", "1.5").
var validTimestamp = regexp.MustCompile(`^(?:\d{2}:\d{2}:\d{2}(?:\.\d+)?|\d+(?:\.\d+)?)$`)

// isFFmpegAvailable returns true if the ffmpeg binary can be found on PATH.
func isFFmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// extractVideoThumbnailFromSource owns the reader opened for this extraction.
func extractVideoThumbnailFromSource(parent context.Context, open func() (io.ReadCloser, error), maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("video thumbnail: invalid input")
	}
	return withThumbnailCapacity(parent, func(ctx context.Context) ([]byte, error) {
		reader, err := open()
		if err != nil {
			return nil, err
		}
		if reader == nil {
			return nil, errors.New("video thumbnail: invalid input")
		}
		defer reader.Close()
		return extractVideoThumbnailInSlot(ctx, reader, maxBytes)
	})
}

// extractVideoThumbnail writes the video from videoReader to a temporary file,
// then invokes ffmpeg to extract a single representative frame as a JPEG.
// It first tries the frame at 1 s; for very short clips it falls back to 0 s.
func extractVideoThumbnail(parent context.Context, videoReader io.ReadCloser, maxBytes int64) ([]byte, error) {
	if videoReader == nil || maxBytes <= 0 {
		return nil, errors.New("video thumbnail: invalid input")
	}
	return withThumbnailCapacity(parent, func(ctx context.Context) ([]byte, error) {
		return extractVideoThumbnailInSlot(ctx, videoReader, maxBytes)
	})
}

// Capacity covers opening the source as well as copying and decoding it.
func withThumbnailCapacity(parent context.Context, generate func(context.Context) ([]byte, error)) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, thumbnailTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case thumbnailSlots <- struct{}{}:
		defer func() { <-thumbnailSlots }()
	default:
		return nil, errors.New("video thumbnail: processing capacity reached")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return generate(ctx)
}

func extractVideoThumbnailInSlot(ctx context.Context, videoReader io.ReadCloser, maxBytes int64) ([]byte, error) {
	stopClose := context.AfterFunc(ctx, func() { videoReader.Close() })
	defer stopClose()
	maxBytes = min(maxBytes, 512*1024*1024)
	// ffmpeg requires a seekable file for most container formats, so we
	// first buffer the entire stream to a temporary file on disk.
	tmp, err := os.CreateTemp("", "mm-video-thumb-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("video thumbnail: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	n, err := io.Copy(tmp, io.LimitReader(videoReader, maxBytes+1))
	if err != nil {
		tmp.Close()
		return nil, fmt.Errorf("video thumbnail: buffer video: %w", err)
	}
	if n > maxBytes {
		tmp.Close()
		return nil, errors.New("video thumbnail: input limit exceeded")
	}
	if err = tmp.Close(); err != nil {
		return nil, fmt.Errorf("video thumbnail: close temp file: %w", err)
	}

	// First attempt: grab the frame at 1 second.
	out, err := runFFmpegFrameContext(ctx, tmpName, "00:00:01")
	if err != nil || len(out) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Fall back to the very first frame (for clips shorter than 1 s).
		out, err = runFFmpegFrameContext(ctx, tmpName, "00:00:00")
		if err != nil {
			return nil, fmt.Errorf("video thumbnail: ffmpeg: %w", err)
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("video thumbnail: ffmpeg produced empty output")
	}
	return out, nil
}

// runFFmpegFrame invokes ffmpeg to extract exactly one frame at the given
// timestamp from inputPath and returns the raw JPEG bytes written to stdout.
//
// inputPath must be an absolute path produced by os.CreateTemp (trusted).
// timestamp must match HH:MM:SS or a plain number to prevent argument injection.
func runFFmpegFrame(inputPath, timestamp string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), thumbnailTimeout)
	defer cancel()
	return runFFmpegFrameContext(ctx, inputPath, timestamp)
}

type thumbnailOutput struct {
	buffer   bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (b *thumbnailOutput) Len() int { return b.buffer.Len() }

func (b *thumbnailOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *thumbnailOutput) Write(data []byte) (int, error) {
	remaining := b.limit - b.Len()
	if len(data) > remaining {
		n, _ := b.buffer.Write(data[:remaining])
		b.exceeded = true
		b.cancel()
		return n, errors.New("video thumbnail: output limit exceeded")
	}
	return b.buffer.Write(data)
}

func runFFmpegFrameContext(parent context.Context, inputPath, timestamp string) ([]byte, error) {
	if !validTimestamp.MatchString(timestamp) {
		return nil, fmt.Errorf("video thumbnail: invalid timestamp format %q: must be HH:MM:SS or a non-negative number", timestamp)
	}

	if err := parent.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stdout := thumbnailOutput{limit: 4 * 1024 * 1024, cancel: cancel}
	stderr := thumbnailOutput{limit: 64 * 1024, cancel: cancel}
	cmd := exec.CommandContext(ctx,
		"ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-max_alloc", "67108864",
		"-protocol_whitelist", "file,pipe",
		"-threads", "1", "-filter_threads", "1",
		"-y",            // overwrite without prompting
		"-i", inputPath, // input file path (from os.CreateTemp — trusted)
		"-ss", timestamp, // seek position before decoding
		"-vframes", "1", // extract exactly one frame
		"-vf", "scale=1280:720:force_original_aspect_ratio=decrease",
		"-threads", "1",
		"-f", "image2", // force image2 muxer
		"-vcodec", "mjpeg", // encode the frame as JPEG
		"pipe:1", // write result to stdout
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second

	if err := cmd.Run(); err != nil {
		if stdout.exceeded || stderr.exceeded {
			return nil, errors.New("video thumbnail: output limit exceeded")
		}
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		return nil, fmt.Errorf("video thumbnail: ffmpeg failed: %w", err)
	}
	return stdout.Bytes(), nil
}
