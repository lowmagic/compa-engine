// Package benchcase generates a synthetic support case shaped like a
// must-gather, of a requested size, for benchmarks. It is deterministic: a
// seed and a size always give the same tree, byte for byte. Nothing in it
// comes from a customer; names, hosts and messages are made up.
package benchcase

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
)

type Options struct {
	// Bytes is roughly how much to write; the tree comes within one file
	// of it.
	Bytes int64
	// Files is roughly how many small files to write, besides the large
	// logs. Default 100,000.
	Files int
	// LargeLogs is how many node journals to write, which take
	// LargeShare of Bytes between them. Default 3 and 0.45.
	LargeLogs  int
	LargeShare float64
	Seed       uint64
}

// Stats is what Generate wrote.
type Stats struct {
	Files int
	Bytes int64
	// Largest is the path of the largest file, relative to the root.
	Largest      string
	LargestBytes int64
}

// Attachment is the directory Generate writes the must-gather into, as an
// extracted attachment would be.
const Attachment = "0010-must-gather.local.5550123"

// Generate writes the case under root.
func Generate(root string, o Options) (Stats, error) {
	if o.Files == 0 {
		o.Files = 100_000
	}
	if o.LargeLogs == 0 {
		o.LargeLogs = 3
	}
	if o.LargeShare == 0 {
		o.LargeShare = 0.45
	}
	g := &gen{rng: rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b97f4a7c15)), root: filepath.Join(root, Attachment, "quay-io-openshift-release-dev-sha256-0000")}
	large := int64(float64(o.Bytes) * o.LargeShare)
	for i := range o.LargeLogs {
		path := filepath.Join("nodes", fmt.Sprintf("worker-%d", i), "journal")
		if err := g.write(path, large/int64(o.LargeLogs), g.journalLine); err != nil {
			return g.stats, err
		}
	}
	small := (o.Bytes - g.stats.Bytes) / int64(o.Files)
	namespaces := max(o.Files/400, 1)
	for i := range o.Files {
		ns := fmt.Sprintf("openshift-bench-%03d", i%namespaces)
		var path string
		var line func([]byte) []byte
		switch i % 4 {
		case 0, 1:
			pod := fmt.Sprintf("app-%d-%05x", i%7, i)
			path = filepath.Join("namespaces", ns, "pods", pod, "app", "app", "logs", "current.log")
			line = g.podLine
		case 2:
			path = filepath.Join("namespaces", ns, "core", fmt.Sprintf("configmap-%05d.yaml", i))
			line = g.yamlLine
		default:
			path = filepath.Join("namespaces", ns, "apps", fmt.Sprintf("deployment-%05d.yaml", i))
			line = g.yamlLine
		}
		// Sizes vary around the mean, as real files do.
		size := small/2 + g.rng.Int64N(max(small, 1))
		if err := g.write(path, size, line); err != nil {
			return g.stats, err
		}
	}
	return g.stats, nil
}

type gen struct {
	rng   *rand.Rand
	root  string
	stats Stats
	n     int64 // lines written, for timestamps
}

func (g *gen) write(rel string, size int64, line func([]byte) []byte) error {
	path := filepath.Join(g.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 256<<10)
	buf := make([]byte, 0, 512)
	var written int64
	for written < size {
		buf = line(buf[:0])
		if over := written + int64(len(buf)) - size; over > 0 && int64(len(buf)) > over {
			buf = append(buf[:int64(len(buf))-over-1], '\n')
		}
		w.Write(buf)
		written += int64(len(buf))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	g.stats.Files++
	g.stats.Bytes += written
	if written > g.stats.LargestBytes {
		g.stats.Largest, g.stats.LargestBytes = filepath.Join(Attachment, "quay-io-openshift-release-dev-sha256-0000", rel), written
	}
	return nil
}

func (g *gen) stamp(b []byte) []byte {
	g.n++
	s := g.n / 4
	b = append(b, "2026-10-18T"...)
	b = appendPad(b, (s/3600)%24)
	b = append(b, ':')
	b = appendPad(b, (s/60)%60)
	b = append(b, ':')
	b = appendPad(b, s%60)
	b = append(b, '.')
	b = strconv.AppendInt(b, 100000+g.rng.Int64N(900000), 10)
	return append(b, 'Z')
}

func appendPad(b []byte, v int64) []byte {
	if v < 10 {
		b = append(b, '0')
	}
	return strconv.AppendInt(b, v, 10)
}

var units = []string{"kubelet[2211]", "crio[1802]", "NetworkManager[1120]", "systemd[1]", "ovs-vswitchd[1350]"}
var messages = []string{
	"SyncLoop (PLEG): event for pod",
	"Started container",
	"Probe succeeded for",
	"Readiness probe failed for",
	"ListPodSandbox returned",
	"connection to 10.128.0.1:6443 timed out for",
	"image pull complete for",
}

func (g *gen) journalLine(b []byte) []byte {
	b = g.stamp(b)
	b = append(b, " worker "...)
	b = append(b, units[g.rng.IntN(len(units))]...)
	b = append(b, ": "...)
	b = append(b, messages[g.rng.IntN(len(messages))]...)
	b = append(b, " app-"...)
	b = strconv.AppendInt(b, g.rng.Int64N(5000), 16)
	return append(b, '\n')
}

func (g *gen) podLine(b []byte) []byte {
	b = g.stamp(b)
	b = append(b, ` level=info msg="handled request" path=/api/v1/items/`...)
	b = strconv.AppendInt(b, g.rng.Int64N(1_000_000), 10)
	b = append(b, " duration_ms="...)
	b = strconv.AppendInt(b, g.rng.Int64N(900), 10)
	return append(b, '\n')
}

var keys = []string{"name", "namespace", "uid", "resourceVersion", "image", "replicas", "value", "status"}

func (g *gen) yamlLine(b []byte) []byte {
	for range g.rng.IntN(4) {
		b = append(b, "  "...)
	}
	b = append(b, keys[g.rng.IntN(len(keys))]...)
	b = append(b, ": v"...)
	b = strconv.AppendUint(b, g.rng.Uint64(), 36)
	return append(b, '\n')
}
