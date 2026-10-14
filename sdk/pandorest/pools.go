package main

import (
	"fmt"

	"github.com/katbyte/pandorest/importer/workarounds"
	"github.com/katbyte/pandorest/openapi"
)

// A pool's scrub, its expansion, its auto-trim setting and its disk layout are described as values of no shape, and a NAS writes each the same way
// every time: these are those shapes, as the TrueNAS source writes them (plugins/pool_/topology.py, api/v25_10_5/pool.py) and a real NAS answered.

// poolShapes are the records declared for them, which the places that leave them open are pointed at.
var poolShapes = map[string]*openapi.Schema{
	"PoolScan": record("A scrub or resilver: the last one, or the one running.", map[string]*openapi.Schema{
		"function":         said(text(), "SCRUB or RESILVER"),
		"state":            said(text(), "SCANNING while it runs, then FINISHED or CANCELED"),
		"start_time":       said(moment(), "when it started"),
		"end_time":         said(moment(), "when it ended; none while it runs"),
		"percentage":       said(number(), "how far it has got"),
		"bytes_to_process": said(integer(), "how much it has to read, in bytes"),
		"bytes_processed":  said(integer(), "how much it has read, in bytes"),
		"bytes_issued":     said(integer(), "how much it has asked the disks for, in bytes"),
		"pause":            said(&openapi.Schema{}, "when it was paused, while it is"),
		"errors":           said(integer(), "how many errors it found"),
		"total_secs_left":  said(integer(), "how long it expects to go on, in seconds; none when it cannot tell"),
	}),
	"PoolExpand": record("A RAIDZ expansion: the last one, or the one running.", map[string]*openapi.Schema{
		"state":                said(text(), "EXPANDING while it runs, then FINISHED"),
		"expanding_vdev":       said(integer(), "which vdev is being widened"),
		"start_time":           said(moment(), "when it started"),
		"end_time":             said(moment(), "when it ended"),
		"bytes_to_reflow":      said(integer(), "how much has to be moved, in bytes"),
		"bytes_reflowed":       said(integer(), "how much has been moved, in bytes"),
		"waiting_for_resilver": said(integer(), "1 while it waits for a resilver to finish"),
		"total_secs_left":      said(integer(), "how long it expects to go on, in seconds"),
		"percentage":           said(number(), "how far it has got"),
	}),
	"PoolAutotrim": record("Whether the pool trims its disks as space is freed, as a ZFS property.", map[string]*openapi.Schema{
		"parsed":   {},
		"rawvalue": text(),
		"source":   said(text(), "where the value comes from: DEFAULT, LOCAL"),
		"value":    said(text(), "on or off"),
	}),
	"PoolVdev": record("A vdev of a pool, or a disk in one: a group of disks with the disks under it, or one disk alone.", map[string]*openapi.Schema{
		"name":         said(text(), "the vdev's name (mirror-0, raidz2-0) or the disk's partition"),
		"type":         said(text(), "MIRROR, RAIDZ1, RAIDZ2, RAIDZ3, DRAID1, DRAID2, DRAID3, STRIPE, DISK, FILE"),
		"path":         said(text(), "the partition's path, for a disk; none for a group"),
		"guid":         said(text(), "ZFS's own number for it"),
		"status":       said(text(), "ONLINE, DEGRADED, FAULTED, OFFLINE, UNAVAIL, REMOVED"),
		"stats":        ref("PoolVdevStats"),
		"children":     listOf(ref("PoolVdev")),
		"device":       said(text(), "the partition the disk is used by, as the system names it"),
		"disk":         said(text(), "the disk, as the system names it"),
		"unavail_disk": said(ref("DiskQueryResultItem"), "for a disk that is not ONLINE, the disk the NAS last knew by its number, if any"),
	}),
	"PoolVdevStats": record("What ZFS counts of a vdev or a disk since the pool was imported.", map[string]*openapi.Schema{
		"timestamp":         integer(),
		"read_errors":       said(integer(), "reads that failed"),
		"write_errors":      said(integer(), "writes that failed"),
		"checksum_errors":   said(integer(), "reads that came back other than what was written"),
		"ops":               said(listOf(integer()), "operations, by kind"),
		"bytes":             said(listOf(integer()), "bytes moved, by kind"),
		"size":              said(integer(), "how big it is, in bytes; 0 for a disk in a group"),
		"allocated":         said(integer(), "how much of it is used, in bytes"),
		"fragmentation":     said(integer(), "how broken up its free space is, in per cent"),
		"self_healed":       said(integer(), "bytes ZFS repaired from a good copy"),
		"configured_ashift": integer(),
		"logical_ashift":    integer(),
		"physical_ashift":   integer(),
		"draid_spare_disks": said(integer(), "for a dRAID vdev, its spare disks"),
		"draid_data_disks":  said(integer(), "for a dRAID vdev, its data disks in a group"),
		"draid_parity":      said(integer(), "for a dRAID vdev, its parity"),
	}),
}

// poolFields are the places that leave the shapes open, and the shape each takes.
var poolFields = map[string]*openapi.Schema{
	"PoolQueryResultItem.scan":     ref("PoolScan"),
	"PoolQueryResultItem.expand":   ref("PoolExpand"),
	"PoolQueryResultItem.autotrim": ref("PoolAutotrim"),
	"BootGetStateAnswer.scan":      ref("PoolScan"),
	"BootGetStateAnswer.expand":    ref("PoolExpand"),
	"BootGetStateAnswer.autotrim":  ref("PoolAutotrim"),
}

// vdevLists points a pool's lists of vdevs, which are described as lists of values of no type, at the vdev record.
type vdevLists struct{}

func (vdevLists) Name() string    { return "truenas-vdev-lists" }
func (vdevLists) Service() string { return "truenas" }

func (vdevLists) Bug() string {
	return "a pool's layout describes each of its lists of vdevs (data, log, cache, spare, special, dedup) as a list of values of no type, and a NAS writes every vdev the same way"
}

func (vdevLists) Apply(spec *openapi.Spec) error {
	for _, list := range []string{"cache", "data", "dedup", "log", "spare", "special"} {
		p, err := workarounds.Property(spec, "PoolTopology", list)
		if err != nil {
			return err
		}
		if p.Type != openapi.TypeArray || p.Items == nil || p.Items.Type != "" || p.Items.RefName() != "" || len(p.Items.Properties) > 0 {
			return fmt.Errorf("PoolTopology.%s has a shape for its vdevs now", list)
		}
		p.Items = ref("PoolVdev")
	}

	return nil
}

func record(description string, properties map[string]*openapi.Schema) *openapi.Schema {
	return &openapi.Schema{Type: openapi.TypeObject, Description: description, Properties: properties}
}

func said(s *openapi.Schema, description string) *openapi.Schema {
	s.Description = description

	return s
}

func text() *openapi.Schema    { return &openapi.Schema{Type: openapi.TypeString} }
func integer() *openapi.Schema { return &openapi.Schema{Type: openapi.TypeInteger} }
func number() *openapi.Schema  { return &openapi.Schema{Type: openapi.TypeNumber} }

// moment is a moment as a NAS writes one, {"$date": milliseconds}, or none.
func moment() *openapi.Schema {
	return &openapi.Schema{Type: openapi.TypeString, Format: "date-time", Nullable: true}
}

func ref(name string) *openapi.Schema { return &openapi.Schema{Ref: openapi.SchemaRefPrefix + name} }

func listOf(items *openapi.Schema) *openapi.Schema {
	return &openapi.Schema{Type: openapi.TypeArray, Items: items}
}
