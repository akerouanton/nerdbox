package task

import (
	"context"
	"fmt"
	"strconv"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
	"github.com/containerd/nerdbox/internal/vm"
)

const (
	resourceAnnotation = "io.containerd.nerdbox.resource"
	cpuAnnotation      = resourceAnnotation + ".cpu"
	memAnnotation      = resourceAnnotation + ".memory"
)

type resourceConfig struct {
	CPU uint8
	Mem uint32
}

func (r *resourceConfig) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	if b.Spec.Annotations == nil {
		return nil
	}

	r.CPU = 2
	r.Mem = 2048

	for annotKey, annotValue := range b.Spec.Annotations {
		switch annotKey {
		case cpuAnnotation:
			cpu, err := strconv.ParseUint(annotValue, 10, 8)
			if err != nil {
				return fmt.Errorf("failed to parse cpu annotation: %w", err)
			}
			r.CPU = uint8(cpu)
			delete(b.Spec.Annotations, annotKey)
		case memAnnotation:
			mem, err := strconv.ParseUint(annotValue, 10, 32)
			if err != nil {
				return fmt.Errorf("failed to parse memory annotation: %w", err)
			}
			r.Mem = uint32(mem)
			delete(b.Spec.Annotations, annotKey)
		}
	}

	return nil
}

func (r *resourceConfig) SetupVM(ctx context.Context, vmi vm.Instance) error {
	if err := vmi.SetCPUAndMemory(ctx, r.CPU, r.Mem); err != nil {
		return fmt.Errorf("failed to set cpu and memory: %w", err)
	}
	return nil
}
