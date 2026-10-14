package queue

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAnArtifactIsStoredWithTheReport(t *testing.T) {
	q, ctx := open(t), context.Background()
	if _, err := q.Artifact(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("artifact of an unknown job: %v, want ErrNotFound", err)
	}
	id, _ := q.Submit(ctx, []string{"make"})
	if _, err := q.Artifact(ctx, id); !errors.Is(err, ErrNotDone) {
		t.Fatalf("artifact of a queued job: %v, want ErrNotDone", err)
	}
	job, err := q.Claim(ctx, "w1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Artifact(ctx, id); !errors.Is(err, ErrNotDone) {
		t.Fatalf("artifact of a running job: %v, want ErrNotDone", err)
	}

	tarball := bytes.Repeat([]byte{0, 1, 2}, 1000)
	if err := q.Report(ctx, job, Result{ExitCode: 0, Artifact: tarball}); err != nil {
		t.Fatal(err)
	}
	got, err := q.Artifact(ctx, id)
	if err != nil || !bytes.Equal(got, tarball) {
		t.Fatalf("artifact = %d bytes, %v; want the %d reported", len(got), err, len(tarball))
	}
	s, _ := q.Get(ctx, id)
	if s.Result.ArtifactBytes != int64(len(tarball)) || s.Result.Artifact != nil {
		t.Errorf("Get: ArtifactBytes %d, Artifact %d bytes; want the size and not the bytes",
			s.Result.ArtifactBytes, len(s.Result.Artifact))
	}
	list, _ := q.List(ctx, "", 0, 10)
	if len(list) != 1 || list[0].Result.ArtifactBytes != int64(len(tarball)) {
		t.Errorf("List: %+v, want the artifact's size", list)
	}
}

func TestAJobWithoutAnArtifactHasNone(t *testing.T) {
	q, ctx := open(t), context.Background()
	id, _ := q.Submit(ctx, []string{"true"})
	job, _ := q.Claim(ctx, "w1", time.Minute)
	if err := q.Report(ctx, job, Result{ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	got, err := q.Artifact(ctx, id)
	if err != nil || got != nil {
		t.Fatalf("artifact = %v, %v; want nil, nil", got, err)
	}
	if s, _ := q.Get(ctx, id); s.Result.ArtifactBytes != 0 {
		t.Errorf("ArtifactBytes = %d, want 0", s.Result.ArtifactBytes)
	}
}
