/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package createusb

// This file owns the p1 live-ISO placeholder stage. It initializes the GPT and reserves
// the planned p1 region without writing ISO bytes. The later ISO stage can extend this
// file while the media assembly caller retains the same partition-stage boundary.

import (
	"fmt"

	"github.com/sirupsen/logrus"
)

const (
	// This provisional 2 GiB slot reserves room for a future verified RHCOS live ISO.
	defaultP1ReservationBytes = int64(2) << 30
	labelLiveISO              = "live-iso"
)

func createP1Partition(log *logrus.Logger, plan PartitionPlan, dev string, deps mediaBuildDeps) error {
	p1, ok := plannedPartition(plan, labelLiveISO)
	if !ok || p1.Number != 1 {
		return fmt.Errorf("p1 (%s) not found at partition number 1 in the media layout", labelLiveISO)
	}
	log.Infof("create-usb: creating placeholder p%d (%s, %d bytes)", p1.Number, p1.Label, p1.Size)
	if err := deps.partition.CreateBaseLayout(dev, plan); err != nil {
		return fmt.Errorf("creating base GPT and placeholder p1 on %s: %w", dev, err)
	}
	return nil
}

// CreateBaseLayout zeroes any existing table, creates a fresh GPT at the full container
// size, and adds the placeholder p1 entry reserving the live-ISO region. The p2/p3/p4
// entries are added separately into the offsets reserved by the plan.
func (p *opsPartitionOps) CreateBaseLayout(dev string, plan PartitionPlan) error {
	if len(plan.Partitions) == 0 || plan.Partitions[0].Number != 1 || plan.Partitions[0].Label != labelLiveISO {
		return fmt.Errorf("media layout must start with p1 (%s)", labelLiveISO)
	}
	p1 := plan.Partitions[0]
	startSector := p1.Offset / sectorSize
	endSector := (p1.Offset+p1.Size)/sectorSize - 1

	// Zap any residual table so the backup GPT lands at the true end of the full-size device.
	if _, err := p.ops.RunInHostNamespace("sgdisk", "--zap-all", dev); err != nil {
		return fmt.Errorf("zapping partition table on %s: %w", dev, err)
	}
	if _, err := p.ops.RunInHostNamespace("sgdisk",
		fmt.Sprintf("--new=%d:%d:%d", p1.Number, startSector, endSector),
		fmt.Sprintf("--change-name=%d:%s", p1.Number, p1.Label),
		fmt.Sprintf("--typecode=%d:%s", p1.Number, placeholderTypeCode),
		dev); err != nil {
		return fmt.Errorf("creating placeholder p1 on %s: %w", dev, err)
	}
	return nil
}
