// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resourcegroup

import (
	rmpb "github.com/pingcap/kvproto/pkg/resource_manager"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
)

// MaxGroupNameLength is max length of the name of a resource group
const MaxGroupNameLength = 32

// NewGroupFromOptions creates a new resource group from the given options.
func NewGroupFromOptions(groupName string, options *model.ResourceGroupSettings) (*rmpb.ResourceGroup, error) {
	if options == nil {
		return nil, ErrInvalidGroupSettings
	}
	if len(groupName) > MaxGroupNameLength {
		return nil, ErrTooLongResourceGroupName
	}

	group := &rmpb.ResourceGroup{
		Name: groupName,
	}

	group.Priority = uint32(options.Priority)
	if options.Runaway != nil {
		if options.Runaway.ExecElapsedTimeMs == 0 && options.Runaway.ProcessedKeys == 0 && options.Runaway.RequestUnit == 0 {
			return nil, ErrResourceGroupRunawayRuleIsEmpty
		}
		runaway := &rmpb.RunawaySettings{
			Rule: &rmpb.RunawayRule{},
		}
		// Update the rule settings.
		runaway.Rule.ExecElapsedTimeMs = options.Runaway.ExecElapsedTimeMs
		runaway.Rule.ProcessedKeys = options.Runaway.ProcessedKeys
		runaway.Rule.RequestUnit = options.Runaway.RequestUnit
		// Update the action settings.
		if options.Runaway.Action == ast.RunawayActionNone {
			return nil, ErrUnknownResourceGroupRunawayAction
		}
		runaway.Action = rmpb.RunawayAction(options.Runaway.Action)
		if options.Runaway.Action == ast.RunawayActionSwitchGroup && len(options.Runaway.SwitchGroupName) == 0 {
			return nil, ErrUnknownResourceGroupRunawaySwitchGroupName
		}
		// TODO: validate the switch group name to ensure it exists.
		runaway.SwitchGroupName = options.Runaway.SwitchGroupName
		// Update the watch settings.
		if options.Runaway.WatchType != ast.WatchNone {
			runaway.Watch = &rmpb.RunawayWatch{}
			runaway.Watch.Type = rmpb.RunawayWatchType(options.Runaway.WatchType)
			runaway.Watch.LastingDurationMs = options.Runaway.WatchDurationMs
		}

		group.RunawaySettings = runaway
	}

	if options.Background != nil {
		group.BackgroundSettings = &rmpb.BackgroundSettings{
			JobTypes:         options.Background.JobTypes,
			UtilizationLimit: options.Background.ResourceUtilLimit,
		}
	}

	if options.RURate > 0 {
		group.Mode = rmpb.GroupMode_RUMode
		group.RUSettings = &rmpb.GroupRequestUnitSettings{
			RU: &rmpb.TokenBucket{
				Settings: &rmpb.TokenLimitSettings{
					FillRate:   options.RURate,
					BurstLimit: options.BurstLimit,
				},
			},
		}
		if len(options.CPULimiter) > 0 || len(options.IOReadBandwidth) > 0 || len(options.IOWriteBandwidth) > 0 {
			return nil, ErrInvalidResourceGroupDuplicatedMode
		}
		return group, nil
	}

	// Only support RU mode now
	return nil, ErrUnknownResourceGroupMode
}
