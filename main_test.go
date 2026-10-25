package main

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/pompon0/pompon0-qwen/utils"
	"github.com/pompon0/pompon0-qwen/utils/scope"
)

func TestCodeErrorCancelsScope(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
				s.SpawnBg(func() error { return codeError(128 + int(sig)) })
				_, err := utils.Recv(ctx, make(chan struct{}))
				return err
			})
			codeErr, ok := errors.AsType[codeError](fmt.Errorf("wrapped: %w", err))
			if !ok || codeErr.exitCode() != 128+int(sig) {
				t.Fatalf("unexpected signal result: %v", err)
			}
		})
	}
}
