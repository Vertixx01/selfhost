package pipeline

import (
	"context"
	"testing"
	"time"

	"torclient/source/transcode"
)

func TestAcquireEncoderPrefersGPUThenCPU(t *testing.T) {
	ctx := context.Background()
	p := &Pipeline{gpuSlots: slots(1), ffmpegSlots: slots(1)}

	enc1, slot1, err := p.acquireEncoder(ctx)
	if err != nil || enc1 != transcode.EncoderNVENC {
		t.Fatalf("first: %q %v", enc1, err)
	}
	enc2, slot2, _ := p.acquireEncoder(ctx)
	if enc2 != transcode.EncoderCPU {
		t.Fatalf("second (GPU busy): %q", enc2)
	}

	// Both busy: the third waits, and gets whichever frees first.
	got := make(chan string, 1)
	go func() {
		enc, _, _ := p.acquireEncoder(ctx)
		got <- enc
	}()
	select {
	case e := <-got:
		t.Fatalf("third didn't wait: %q", e)
	case <-time.After(50 * time.Millisecond):
	}
	release(slot2)
	if e := <-got; e != transcode.EncoderCPU {
		t.Fatalf("third got %q after CPU slot freed", e)
	}
	release(slot1)

	// Cancelled while waiting.
	full := &Pipeline{gpuSlots: slots(1)}
	full.gpuSlots <- struct{}{}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, _, err := full.acquireEncoder(cctx); err == nil {
		t.Fatal("no error after cancel")
	}

	// CPU-only machine: never GPU.
	cpuOnly := &Pipeline{ffmpegSlots: slots(2)}
	if enc, _, _ := cpuOnly.acquireEncoder(ctx); enc != transcode.EncoderCPU {
		t.Fatalf("cpu-only got %q", enc)
	}
}
