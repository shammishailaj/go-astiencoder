package astilibav

import "C"
import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"text/template"
	"unsafe"

	"github.com/asticode/go-astiencoder"
	"github.com/asticode/go-astitools/stat"
	"github.com/asticode/go-astitools/sync"
	"github.com/asticode/go-astitools/worker"
	"github.com/asticode/goav/avcodec"
	"github.com/pkg/errors"
)

var countPktDumper uint64

// PktDumper represents an object capable of dumping packets
type PktDumper struct {
	*astiencoder.BaseNode
	count            uint32
	data             map[string]interface{}
	e                astiencoder.EmitEventFunc
	fn               PktDumpFunc
	pattern          string
	q                *astisync.CtxQueue
	statIncomingRate *astistat.IncrementStat
	statWorkRatio    *astistat.DurationRatioStat
	t                *template.Template
}

// PktDumpFunc represents a pkt dump func
type PktDumpFunc func(pkt *avcodec.Packet, pattern string) error

// NewPktDumper creates a new pk dumper
func NewPktDumper(pattern string, fn PktDumpFunc, data map[string]interface{}, e astiencoder.EmitEventFunc) (d *PktDumper, err error) {
	// Create pkt dumper
	count := atomic.AddUint64(&countPktDumper, uint64(1))
	d = &PktDumper{
		BaseNode: astiencoder.NewBaseNode(e, astiencoder.NodeMetadata{
			Description: "Dump packets",
			Label:       fmt.Sprintf("Pkt dumper #%d", count),
			Name:        fmt.Sprintf("pkt_dumper_%d", count),
		}),
		data:             data,
		e:                e,
		fn:               fn,
		pattern:          pattern,
		q:                astisync.NewCtxQueue(),
		statIncomingRate: astistat.NewIncrementStat(),
		statWorkRatio:    astistat.NewDurationRatioStat(),
	}
	d.addStats()

	// Parse pattern
	if d.t, err = template.New("").Parse(pattern); err != nil {
		err = errors.Wrapf(err, "astilibav: parsing pattern %s as template failed", pattern)
		return
	}
	return
}

func (d *PktDumper) addStats() {
	// Add incoming rate
	d.Stater().AddStat(astistat.StatMetadata{
		Description: "Number of packets coming in the pkt dumper per second",
		Label:       "Incoming rate",
		Unit:        "pps",
	}, d.statIncomingRate)

	// Add work ratio
	d.Stater().AddStat(astistat.StatMetadata{
		Description: "Percentage of time spent doing some actual work",
		Label:       "Work ratio",
		Unit:        "%",
	}, d.statWorkRatio)

	// Add queue stats
	d.q.AddStats(d.Stater())
}

// Start starts the pkt dumper
func (d *PktDumper) Start(ctx context.Context, t astiencoder.CreateTaskFunc) {
	d.BaseNode.Start(ctx, t, func(t *astiworker.Task) {
		// Handle context
		go d.q.HandleCtx(d.Context())

		// Make sure to stop the queue properly
		defer d.q.Stop()

		// Start queue
		d.q.Start(func(p interface{}) {
			// Handle pause
			defer d.HandlePause()

			// Assert payload
			pkt := p.(*avcodec.Packet)

			// Increment incoming rate
			d.statIncomingRate.Add(1)

			// Increment count
			c := atomic.AddUint32(&d.count, 1)

			// Create data
			d.data["count"] = c
			d.data["pts"] = pkt.Pts()
			d.data["stream_idx"] = pkt.StreamIndex()

			// Execute template
			buf := &bytes.Buffer{}
			d.statWorkRatio.Add(true)
			if err := d.t.Execute(buf, d.data); err != nil {
				d.statWorkRatio.Done(true)
				d.e(astiencoder.EventError(errors.Wrapf(err, "astilibav: executing template %s with data %+v failed", d.pattern, d.data)))
				return
			}
			d.statWorkRatio.Done(true)

			// Dump
			d.statWorkRatio.Add(true)
			if err := d.fn(pkt, buf.String()); err != nil {
				d.statWorkRatio.Done(true)
				d.e(astiencoder.EventError(errors.Wrapf(err, "astilibav: pkt dump func with pattern %s failed", buf)))
				return
			}
			d.statWorkRatio.Done(true)
		})
	})
}

// HandlePkt implements the PktHandler interface
func (d *PktDumper) HandlePkt(pkt *avcodec.Packet) {
	d.q.Send(pkt)
}

// PktDumpFunc is a PktDumpFunc that dumps the packet to a file
var PktDumpFile = func(pkt *avcodec.Packet, pattern string) (err error) {
	// Create file
	var f *os.File
	if f, err = os.Create(pattern); err != nil {
		err = errors.Wrapf(err, "astilibav: creating file %s failed", pattern)
		return
	}
	defer f.Close()

	// Write to file
	if _, err = f.Write(C.GoBytes(unsafe.Pointer(pkt.Data()), (C.int)(pkt.Size()))); err != nil {
		err = errors.Wrapf(err, "astilibav: writing to file %s failed", pattern)
		return
	}
	return
}
