/*
 * Copyright 2023 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this inputFilePath except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package precache

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/sirupsen/logrus"
)

// Progress represents the progress tracking data for the precaching job
type Progress struct {
	Total          int      `json:"total"`
	Pulled         int      `json:"pulled"`
	Failed         int      `json:"failed"`
	Skipped        int      `json:"skipped"`
	FailedPullList []string `json:"failed_pulls"`
	mux            sync.Mutex
}

func (p *Progress) Update(success bool, image string) {
	p.mux.Lock()
	defer p.mux.Unlock()

	if success {
		p.Pulled++
	} else {
		p.Failed++
		if p.FailedPullList == nil {
			p.FailedPullList = []string{}
		}
		p.FailedPullList = append(p.FailedPullList, image)
	}
}

func (p *Progress) Log() {
	logrus.Infof("Total Images: %d", p.Total)
	logrus.Infof("Images Pulled Successfully: %d", p.Pulled)
	logrus.Infof("Images Skipped: %d", p.Skipped)
	logrus.Infof("Images Failed to Pull: %d", p.Failed)
	for _, img := range p.FailedPullList {
		logrus.Infof("failed: %s", img)
	}
}

func (p *Progress) Persist(filename string) {
	data, _ := json.Marshal(p)
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		logrus.Errorf("Failed to update progress file for precaching, err: %v", err)
	}
}
