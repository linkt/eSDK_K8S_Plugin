/*
 *  Copyright (c) Huawei Technologies Co., Ltd. 2020-2022. All rights reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *       http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package volume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"huawei-csi-driver/storage/oceanstor/client"
	"huawei-csi-driver/storage/oceanstor/smartx"
	"huawei-csi-driver/utils"
	"huawei-csi-driver/utils/log"
	"huawei-csi-driver/utils/taskflow"
)

type SAN struct {
	Base
}

func NewSAN(cli, metroRemoteCli, replicaRemoteCli client.BaseClientInterface, product string) *SAN {
	return &SAN{
		Base: Base{
			cli:              cli,
			metroRemoteCli:   metroRemoteCli,
			replicaRemoteCli: replicaRemoteCli,
			product:          product,
		},
	}
}

func (p *SAN) preCreate(ctx context.Context, params map[string]interface{}) error {
	err := p.commonPreCreate(ctx, params)
	if err != nil {
		return err
	}

	name := params["name"].(string)
	params["name"] = utils.GetLunName(name)

	if v, exist := params["sourcevolumename"].(string); exist {
		params["clonefrom"] = utils.GetLunName(v)
	} else if v, exist := params["sourcesnapshotname"].(string); exist {
		params["fromSnapshot"] = utils.GetSnapshotName(v)
	} else if v, exist := params["clonefrom"].(string); exist {
		params["clonefrom"] = utils.GetLunName(v)
	}

	err = p.setWorkLoadID(ctx, p.cli, params)
	if err != nil {
		return err
	}

	return nil
}

func (p *SAN) Create(ctx context.Context, params map[string]interface{}) (utils.Volume, error) {
	err := p.preCreate(ctx, params)
	if err != nil {
		return nil, err
	}

	taskflow := taskflow.NewTaskFlow(ctx, "Create-LUN-Volume")

	replication, replicationOK := params["replication"].(bool)
	hyperMetro, hyperMetroOK := params["hypermetro"].(bool)
	if (replicationOK && replication) && (hyperMetroOK && hyperMetro) {
		msg := "cannot create replication and hypermetro for a volume at the same time"
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	} else if replicationOK && replication {
		taskflow.AddTask("Get-Replication-Params", p.getReplicationParams, nil)
	} else if hyperMetroOK && hyperMetro {
		taskflow.AddTask("Get-HyperMetro-Params", p.getHyperMetroParams, nil)
	}

	taskflow.AddTask("Create-Local-LUN", p.createLocalLun, p.revertLocalLun)
	taskflow.AddTask("Create-Local-QoS", p.createLocalQoS, p.revertLocalQoS)

	if replicationOK && replication {
		taskflow.AddTask("Create-Remote-LUN", p.createRemoteLun, p.revertRemoteLun)
		taskflow.AddTask("Create-Remote-QoS", p.createRemoteQoS, p.revertRemoteQoS)
		taskflow.AddTask("Create-Replication-Pair", p.createReplicationPair, nil)
	} else if hyperMetroOK && hyperMetro {
		taskflow.AddTask("Create-Remote-LUN", p.createRemoteLun, p.revertRemoteLun)
		taskflow.AddTask("Create-Remote-QoS", p.createRemoteQoS, p.revertRemoteQoS)
		taskflow.AddTask("Create-HyperMetro", p.createHyperMetro, p.revertHyperMetro)
	}

	res, err := taskflow.Run(params)
	if err != nil {
		taskflow.Revert()
		return nil, err
	}

	volObj := p.prepareVolObj(ctx, params, res)
	return volObj, nil
}

func (p *SAN) Delete(ctx context.Context, name string) error {
	lunName := utils.GetLunName(name)
	lun, err := p.cli.GetLunByName(ctx, lunName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get lun by name %s error: %v", lunName, err)
		return err
	}
	if lun == nil {
		log.AddContext(ctx).Infof("Lun %s to delete does not exist", lunName)
		return nil
	}

	rssStr := lun["HASRSSOBJECT"].(string)

	var rss map[string]string
	json.Unmarshal([]byte(rssStr), &rss)

	taskflow := taskflow.NewTaskFlow(ctx, "Delete-LUN-Volume")

	if rss["HyperMetro"] == "TRUE" {
		taskflow.AddTask("Delete-HyperMetro", p.deleteHyperMetro, nil)
		taskflow.AddTask("Delete-HyperMetro-Remote-LUN", p.deleteHyperMetroRemoteLun, nil)
	}

	if rss["RemoteReplication"] == "TRUE" {
		taskflow.AddTask("Delete-Replication-Pair", p.deleteReplicationPair, nil)
		taskflow.AddTask("Delete-Replication-Remote-LUN", p.deleteReplicationRemoteLun, nil)
	}

	if rss["LunCopy"] == "TRUE" {
		taskflow.AddTask("Delete-Local-LunCopy", p.deleteLocalLunCopy, nil)
	}

	if rss["HyperCopy"] == "TRUE" {
		taskflow.AddTask("Delete-Local-HyperCopy", p.deleteLocalHyperCopy, nil)
	}

	taskflow.AddTask("Delete-Local-LUN", p.deleteLocalLun, nil)

	params := map[string]interface{}{
		"lun":     lun,
		"lunID":   lun["ID"].(string),
		"lunName": lunName,
	}

	_, err = taskflow.Run(params)
	return err
}

func (p *SAN) Expand(ctx context.Context, name string, newSize int64) (bool, error) {
	lunName := utils.GetLunName(name)
	lun, err := p.cli.GetLunByName(ctx, lunName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get lun by name %s error: %v", lunName, err)
		return false, err
	} else if lun == nil {
		msg := fmt.Sprintf("Lun %s to expand does not exist", lunName)
		log.AddContext(ctx).Errorf(msg)
		return false, errors.New(msg)
	}

	isAttached := lun["EXPOSEDTOINITIATOR"] == "true"
	curSize, _ := strconv.ParseInt(lun["CAPACITY"].(string), 10, 64)
	if newSize <= curSize {
		msg := fmt.Sprintf("Lun %s newSize %d must be greater than curSize %d", lunName, newSize, curSize)
		log.AddContext(ctx).Errorln(msg)
		return false, errors.New(msg)
	}

	var rss map[string]string
	json.Unmarshal([]byte(lun["HASRSSOBJECT"].(string)), &rss)

	expandTask := taskflow.NewTaskFlow(ctx, "Expand-LUN-Volume")
	expandTask.AddTask("Expand-PreCheck-Capacity", p.preExpandCheckCapacity, nil)

	if rss["HyperMetro"] == "TRUE" {
		expandTask.AddTask("Expand-HyperMetro-Remote-PreCheck-Capacity",
			p.preExpandHyperMetroCheckRemoteCapacity, nil)
		expandTask.AddTask("Suspend-HyperMetro", p.suspendHyperMetro, nil)
		expandTask.AddTask("Expand-HyperMetro-Remote-LUN", p.expandHyperMetroRemoteLun, nil)
	}

	if rss["RemoteReplication"] == "TRUE" {
		expandTask.AddTask("Expand-Replication-Remote-PreCheck-Capacity",
			p.preExpandReplicationCheckRemoteCapacity, nil)
		expandTask.AddTask("Split-Replication", p.splitReplication, nil)
		expandTask.AddTask("Expand-Replication-Remote-LUN", p.expandReplicationRemoteLun, nil)
	}

	expandTask.AddTask("Expand-Local-Lun", p.expandLocalLun, nil)

	if rss["HyperMetro"] == "TRUE" {
		expandTask.AddTask("Sync-HyperMetro", p.syncHyperMetro, nil)
	}

	if rss["RemoteReplication"] == "TRUE" {
		expandTask.AddTask("Sync-Replication", p.syncReplication, nil)
	}

	params := map[string]interface{}{
		"name":            name,
		"size":            newSize,
		"expandSize":      newSize - curSize,
		"lunID":           lun["ID"].(string),
		"localParentName": lun["PARENTNAME"].(string),
	}
	_, err = expandTask.Run(params)
	return isAttached, err
}

func (p *SAN) createLocalLun(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunName := params["name"].(string)

	lun, err := p.cli.GetLunByName(ctx, lunName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get LUN %s error: %v", lunName, err)
		return nil, err
	}

	if lun == nil {
		params["parentid"] = params["poolID"].(string)

		if _, exist := params["clonefrom"]; exist {
			lun, err = p.clone(ctx, params, taskResult)
		} else if _, exist := params["fromSnapshot"]; exist {
			lun, err = p.createFromSnapshot(ctx, params, taskResult)
		} else {
			lun, err = p.cli.CreateLun(ctx, params)
		}

		if err != nil {
			log.AddContext(ctx).Errorf("Create LUN %s error: %v", lunName, err)
			return nil, err
		}
	} else {
		err := p.waitCloneFinish(ctx, lun, taskResult)
		if err != nil {
			log.AddContext(ctx).Errorf("Wait clone finish for LUN %s error: %v", lunName, err)
			return nil, err
		}
	}

	return map[string]interface{}{
		"localLunID": lun["ID"].(string),
		"lunWWN":     lun["WWN"].(string),
	}, nil
}

func (p *SAN) clonePair(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	cloneFrom := params["clonefrom"].(string)
	srcLun, err := p.cli.GetLunByName(ctx, cloneFrom)
	if err != nil {
		log.AddContext(ctx).Errorf("Get clone src LUN %s error: %v", cloneFrom, err)
		return nil, err
	}
	if srcLun == nil {
		msg := fmt.Sprintf("Clone src LUN %s does not exist", cloneFrom)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	srcLunCapacity, err := strconv.ParseInt(srcLun["CAPACITY"].(string), 10, 64)
	if err != nil {
		return nil, err
	}
	cloneLunCapacity := params["capacity"].(int64)
	if cloneLunCapacity < srcLunCapacity {
		msg := fmt.Sprintf("Clone LUN capacity must be >= src %s", cloneFrom)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	dstLun, err := p.cli.GetLunByName(ctx, params["name"].(string))
	if err != nil {
		return nil, err
	}
	if dstLun == nil {
		copyParams := utils.CopyMap(params)
		copyParams["capacity"] = srcLunCapacity

		dstLun, err = p.cli.CreateLun(ctx, copyParams)
		if err != nil {
			return nil, err
		}
	}
	srcLunID := srcLun["ID"].(string)
	dstLunID := dstLun["ID"].(string)

	cloneSpeed := params["clonespeed"].(int)
	err = p.createClonePair(ctx, clonePairRequest{srcLunID: srcLunID,
		dstLunID:         dstLunID,
		cloneLunCapacity: cloneLunCapacity,
		srcLunCapacity:   srcLunCapacity,
		cloneSpeed:       cloneSpeed})
	if err != nil {
		log.AddContext(ctx).Errorf("Create clone pair, source lun ID %s, target lun ID %s error: %s",
			srcLunID, dstLunID, err)
		p.cli.DeleteLun(ctx, dstLunID)
		return nil, err
	}

	return dstLun, nil
}

func (p *SAN) fromSnapshotByClonePair(ctx context.Context,
	params map[string]interface{}) (map[string]interface{}, error) {
	srcSnapshotName := params["fromSnapshot"].(string)
	srcSnapshot, err := p.cli.GetLunSnapshotByName(ctx, srcSnapshotName)
	if err != nil {
		return nil, err
	}
	if srcSnapshot == nil {
		msg := fmt.Sprintf("Clone snapshot %s does not exist", srcSnapshotName)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	srcSnapshotCapacity, err := strconv.ParseInt(srcSnapshot["USERCAPACITY"].(string), 10, 64)
	if err != nil {
		return nil, err
	}

	cloneLunCapacity := params["capacity"].(int64)
	if cloneLunCapacity < srcSnapshotCapacity {
		msg := fmt.Sprintf("Clone target LUN capacity must be >= src snapshot %s", srcSnapshotName)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	dstLun, err := p.cli.GetLunByName(ctx, params["name"].(string))
	if err != nil {
		return nil, err
	}
	if dstLun == nil {
		copyParams := utils.CopyMap(params)
		copyParams["capacity"] = srcSnapshotCapacity

		dstLun, err = p.cli.CreateLun(ctx, copyParams)
		if err != nil {
			return nil, err
		}
	}

	srcSnapshotID := srcSnapshot["ID"].(string)
	dstLunID := dstLun["ID"].(string)
	cloneSpeed := params["clonespeed"].(int)
	err = p.createClonePair(ctx, clonePairRequest{srcLunID: srcSnapshotID,
		dstLunID:         dstLunID,
		cloneLunCapacity: cloneLunCapacity,
		srcLunCapacity:   srcSnapshotCapacity,
		cloneSpeed:       cloneSpeed})
	if err != nil {
		log.AddContext(ctx).Errorf("Clone snapshot by clone pair, source snapshot ID %s,"+
			" target lun ID %s error: %s", srcSnapshotID, dstLunID, err)

		p.cli.DeleteLun(ctx, dstLunID)
		return nil, err
	}

	return dstLun, nil
}

type clonePairRequest struct {
	srcLunID         string
	dstLunID         string
	cloneLunCapacity int64
	srcLunCapacity   int64
	cloneSpeed       int
}

func (p *SAN) createClonePair(ctx context.Context,
	clonePairReq clonePairRequest) error {
	clonePair, err := p.cli.CreateClonePair(ctx, clonePairReq.srcLunID,
		clonePairReq.dstLunID, clonePairReq.cloneSpeed)
	if err != nil {
		log.AddContext(ctx).Errorf("Create ClonePair from %s to %s error: %v", clonePairReq.srcLunID,
			clonePairReq.dstLunID, err)
		return err
	}

	clonePairID := clonePair["ID"].(string)
	if clonePairReq.srcLunCapacity < clonePairReq.cloneLunCapacity {
		err = p.cli.ExtendLun(ctx, clonePairReq.dstLunID, clonePairReq.cloneLunCapacity)
		if err != nil {
			log.AddContext(ctx).Errorf("Extend clone lun %s error: %v", clonePairReq.dstLunID, err)
			p.cli.DeleteClonePair(ctx, clonePairID)
			return err
		}
	}

	err = p.cli.SyncClonePair(ctx, clonePairID)
	if err != nil {
		log.AddContext(ctx).Errorf("Start ClonePair %s error: %v", clonePairID, err)
		p.cli.DeleteClonePair(ctx, clonePairID)
		return err
	}

	err = p.waitClonePairFinish(ctx, clonePairID)
	if err != nil {
		log.AddContext(ctx).Errorf("Wait ClonePair %s finish error: %v", clonePairID, err)
		return err
	}

	return nil
}

func (p *SAN) lunCopy(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	clonefrom := params["clonefrom"].(string)
	srcLun, err := p.cli.GetLunByName(ctx, clonefrom)
	if err != nil {
		log.AddContext(ctx).Errorf("Get clone src LUN %s error: %v", clonefrom, err)
		return nil, err
	} else if srcLun == nil {
		msg := fmt.Sprintf("Clone src LUN %s does not exist", clonefrom)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	srcLunCapacity, err := strconv.ParseInt(srcLun["CAPACITY"].(string), 10, 64)
	if err != nil {
		return nil, err
	}

	cloneLunCapacity := params["capacity"].(int64)
	if cloneLunCapacity < srcLunCapacity {
		msg := fmt.Sprintf("Clone LUN capacity must be >= src %s", clonefrom)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	dstLun, err := p.cli.GetLunByName(ctx, params["name"].(string))
	if err != nil {
		return nil, err
	} else if dstLun == nil {
		dstLun, err = p.cli.CreateLun(ctx, params)
		if err != nil {
			return nil, err
		}
	}

	srcLunID := srcLun["ID"].(string)
	dstLunID := dstLun["ID"].(string)
	snapshotName := fmt.Sprintf("k8s_lun_%s_to_%s_snap", srcLunID, dstLunID)

	smartX := smartx.NewSmartX(p.cli)
	snapshot, err := p.cli.GetLunSnapshotByName(ctx, snapshotName)
	if err != nil {
		return nil, err
	} else if snapshot == nil {
		snapshot, err = smartX.CreateLunSnapshot(ctx, snapshotName, srcLunID)
		if err != nil {
			log.AddContext(ctx).Errorf("Create snapshot %s error: %v", snapshotName, err)
			p.cli.DeleteLun(ctx, dstLunID)
			return nil, err
		}
	}

	lunCopyName, err := p.ensureLUNCopy(ctx, snapshot["ID"].(string), dstLunID, params["clonespeed"].(int))
	if err != nil {
		return nil, err
	}

	err = p.deleteLunCopy(ctx, lunCopyName, true)
	if err != nil {
		log.AddContext(ctx).Errorf("Delete luncopy %s error: %v", lunCopyName, err)
		return nil, err
	}

	return dstLun, nil
}

func (p *SAN) ensureLUNCopy(ctx context.Context, snapshotID, dstLunID string, cloneSpeed int) (string, error) {
	lunCopyName, err := p.createLunCopy(ctx, snapshotID, dstLunID, cloneSpeed, true)
	if err != nil {
		log.AddContext(ctx).Errorf("Create lun copy, source snapshot ID %s, target lun ID %s error: %s",
			snapshotID, dstLunID, err)
		smartx.NewSmartX(p.cli).DeleteLunSnapshot(ctx, snapshotID)
		p.cli.DeleteLun(ctx, dstLunID)
		return "", err
	}

	err = p.waitLunCopyFinish(ctx, lunCopyName)
	if err != nil {
		log.AddContext(ctx).Errorf("Wait luncopy %s finish error: %v", lunCopyName, err)
		return "", err
	}
	return lunCopyName, nil
}

func (p *SAN) fromSnapshotByLunCopy(ctx context.Context,
	params map[string]interface{}) (map[string]interface{}, error) {
	srcSnapshotName := params["fromSnapshot"].(string)
	srcSnapshot, err := p.cli.GetLunSnapshotByName(ctx, srcSnapshotName)
	if err != nil {
		return nil, err
	}
	if srcSnapshot == nil {
		msg := fmt.Sprintf("Clone src snapshot %s does not exist", srcSnapshotName)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	srcSnapshotCapacity, err := strconv.ParseInt(srcSnapshot["USERCAPACITY"].(string), 10, 64)
	if err != nil {
		return nil, err
	}

	if params["capacity"].(int64) < srcSnapshotCapacity {
		msg := fmt.Sprintf("Clone LUN capacity must be >= src snapshot%s", srcSnapshotName)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	dstLun, err := p.cli.GetLunByName(ctx, params["name"].(string))
	if err != nil {
		return nil, err
	}
	if dstLun == nil {
		dstLun, err = p.cli.CreateLun(ctx, params)
		if err != nil {
			return nil, err
		}
	}

	dstLunID := dstLun["ID"].(string)
	lunCopyName, err := p.createLunCopy(ctx, srcSnapshot["ID"].(string),
		dstLunID, params["clonespeed"].(int), false)
	if err != nil {
		log.AddContext(ctx).Errorf("Create LunCopy, source snapshot ID %s, target lun ID %s error: %s",
			srcSnapshot["ID"].(string), dstLunID, err)
		p.cli.DeleteLun(ctx, dstLunID)
		return nil, err
	}

	err = p.waitLunCopyFinish(ctx, lunCopyName)
	if err != nil {
		log.AddContext(ctx).Errorf("Wait luncopy %s finish error: %v", lunCopyName, err)
		return nil, err
	}

	err = p.deleteLunCopy(ctx, lunCopyName, false)
	if err != nil {
		log.AddContext(ctx).Errorf("Delete luncopy %s error: %v", lunCopyName, err)
		return nil, err
	}

	return dstLun, nil
}

func (p *SAN) createLunCopy(ctx context.Context,
	snapshotID, dstLunID string, cloneSpeed int, isDeleteSnapshot bool) (string, error) {
	lunCopyName := fmt.Sprintf("k8s_luncopy_%s_to_%s", snapshotID, dstLunID)

	lunCopy, err := p.cli.GetLunCopyByName(ctx, lunCopyName)
	if err != nil {
		return "", err
	}

	if lunCopy == nil {
		lunCopy, err = p.cli.CreateLunCopy(ctx, lunCopyName, snapshotID, dstLunID, cloneSpeed)
		if err != nil {
			log.AddContext(ctx).Errorf("Create luncopy from %s to %s error: %v", snapshotID, dstLunID, err)
			return "", err
		}
	}

	lunCopyID := lunCopy["ID"].(string)

	err = p.cli.StartLunCopy(ctx, lunCopyID)
	if err != nil {
		log.AddContext(ctx).Errorf("Start luncopy %s error: %v", lunCopyID, err)
		p.cli.DeleteLunCopy(ctx, lunCopyID)
		return "", err
	}

	return lunCopyName, nil
}

func (p *SAN) clone(ctx context.Context,
	params map[string]interface{}, taskResult map[string]interface{}) (map[string]interface{}, error) {
	if p.product == "DoradoV6" {
		return p.clonePair(ctx, params)
	} else {
		return p.lunCopy(ctx, params)
	}
}

func (p *SAN) createFromSnapshot(ctx context.Context,
	params map[string]interface{}, taskResult map[string]interface{}) (map[string]interface{}, error) {
	if p.product == "DoradoV6" {
		return p.fromSnapshotByClonePair(ctx, params)
	} else {
		return p.fromSnapshotByLunCopy(ctx, params)
	}
}

func (p *SAN) revertLocalLun(ctx context.Context, taskResult map[string]interface{}) error {
	lunID, exist := taskResult["localLunID"].(string)
	if !exist || lunID == "" {
		return nil
	}
	err := p.cli.DeleteLun(ctx, lunID)
	return err
}

func (p *SAN) createLocalQoS(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	qos, exist := params["qos"].(map[string]int)
	if !exist {
		return nil, nil
	}

	lunID := taskResult["localLunID"].(string)
	lun, err := p.cli.GetLunByID(ctx, lunID)
	if err != nil {
		return nil, err
	}

	qosID, exist := lun["IOCLASSID"].(string)
	if !exist || qosID == "" {
		smartX := smartx.NewSmartX(p.cli)
		qosID, err = smartX.CreateQos(ctx, lunID, "lun", "", qos)
		if err != nil {
			log.AddContext(ctx).Errorf("Create qos %v for lun %s error: %v", qos, lunID, err)
			return nil, err
		}
	}

	return map[string]interface{}{
		"localQosID": qosID,
	}, nil
}

func (p *SAN) revertLocalQoS(ctx context.Context, taskResult map[string]interface{}) error {
	lunID, lunIDExist := taskResult["localLunID"].(string)
	qosID, qosIDExist := taskResult["localQosID"].(string)
	if !lunIDExist || !qosIDExist {
		return nil
	}
	smartX := smartx.NewSmartX(p.cli)
	err := smartX.DeleteQos(ctx, qosID, lunID, "lun", "")
	return err
}

func (p *SAN) getLunCopyOfLunID(ctx context.Context, lunID string) (string, error) {
	lun, err := p.cli.GetLunByID(ctx, lunID)
	if err != nil {
		return "", err
	}

	lunCopyIDStr, exist := lun["LUNCOPYIDS"].(string)
	if !exist || lunCopyIDStr == "" {
		return "", nil
	}

	var lunCopyIDs []string

	json.Unmarshal([]byte(lunCopyIDStr), &lunCopyIDs)
	if len(lunCopyIDs) <= 0 {
		return "", nil
	}

	lunCopyID := lunCopyIDs[0]
	lunCopy, err := p.cli.GetLunCopyByID(ctx, lunCopyID)
	if err != nil {
		return "", err
	}

	return lunCopy["NAME"].(string), nil
}

func (p *SAN) deleteLunCopy(ctx context.Context, lunCopyName string, isDeleteSnapshot bool) error {
	lunCopy, err := p.cli.GetLunCopyByName(ctx, lunCopyName)
	if err != nil {
		return err
	}
	if lunCopy == nil {
		return nil
	}

	lunCopyID := lunCopy["ID"].(string)
	runningStatus := lunCopy["RUNNINGSTATUS"].(string)
	if runningStatus == lunCopyRunningStatusQueuing ||
		runningStatus == lunCopyRunningStatusCopying {
		p.cli.StopLunCopy(ctx, lunCopyID)
	}

	err = p.cli.DeleteLunCopy(ctx, lunCopyID)
	if err != nil {
		return err
	}

	snapshotName := lunCopy["SOURCELUNNAME"].(string)
	snapshot, err := p.cli.GetLunSnapshotByName(ctx, snapshotName)
	if err == nil && snapshot != nil && isDeleteSnapshot {
		snapshotID := snapshot["ID"].(string)
		smartX := smartx.NewSmartX(p.cli)
		smartX.DeleteLunSnapshot(ctx, snapshotID)
	}

	return nil
}

func (p *SAN) waitLunCopyFinish(ctx context.Context, lunCopyName string) error {
	err := utils.WaitUntil(func() (bool, error) {
		lunCopy, err := p.cli.GetLunCopyByName(ctx, lunCopyName)
		if err != nil {
			return false, err
		}
		if lunCopy == nil {
			return true, nil
		}

		healthStatus := lunCopy["HEALTHSTATUS"].(string)
		if healthStatus == lunCopyHealthStatusFault {
			return false, fmt.Errorf("Luncopy %s is at fault status", lunCopyName)
		}

		runningStatus := lunCopy["RUNNINGSTATUS"].(string)
		if runningStatus == lunCopyRunningStatusQueuing ||
			runningStatus == lunCopyRunningStatusCopying {
			return false, nil
		} else if runningStatus == lunCopyRunningStatusStop ||
			runningStatus == lunCopyRunningStatusPaused {
			return false, fmt.Errorf("Luncopy %s is stopped", lunCopyName)
		} else {
			return true, nil
		}
	}, time.Hour*6, time.Second*5)

	if err != nil {
		return err
	}

	return nil
}

func (p *SAN) waitClonePairFinish(ctx context.Context, clonePairID string) error {
	err := utils.WaitUntil(func() (bool, error) {
		clonePair, err := p.cli.GetClonePairInfo(ctx, clonePairID)
		if err != nil {
			return false, err
		}
		if clonePair == nil {
			return true, nil
		}

		healthStatus := clonePair["copyStatus"].(string)
		if healthStatus == clonePairHealthStatusFault {
			return false, fmt.Errorf("ClonePair %s is at fault status", clonePairID)
		}

		runningStatus := clonePair["syncStatus"].(string)
		if runningStatus == clonePairRunningStatusNormal {
			return true, nil
		} else if runningStatus == clonePairRunningStatusSyncing ||
			runningStatus == clonePairRunningStatusInitializing ||
			runningStatus == clonePairRunningStatusUnsyncing {
			return false, nil
		} else {
			return false, fmt.Errorf("ClonePair %s running status is abnormal", clonePairID)
		}
	}, time.Hour*6, time.Second*5)

	if err != nil {
		return err
	}

	p.cli.DeleteClonePair(ctx, clonePairID)
	return nil
}

func (p *SAN) waitCloneFinish(ctx context.Context,
	lun map[string]interface{}, taskResult map[string]interface{}) error {
	lunID := lun["ID"].(string)
	if p.product == "DoradoV6" {
		// ID of clone pair is the same as destination LUN ID
		err := p.waitClonePairFinish(ctx, lunID)
		if err != nil {
			return err
		}
	} else {
		lunCopyName, err := p.getLunCopyOfLunID(ctx, lunID)
		if err != nil {
			return err
		}

		if len(lunCopyName) > 0 {
			err := p.waitLunCopyFinish(ctx, lunCopyName)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (p *SAN) createRemoteLun(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunName := params["name"].(string)
	remoteCli := taskResult["remoteCli"].(client.BaseClientInterface)

	lun, err := remoteCli.GetLunByName(ctx, lunName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get remote LUN %s error: %v", lunName, err)
		return nil, err
	}

	if lun == nil {
		err = p.setWorkLoadID(ctx, remoteCli, params)
		if err != nil {
			return nil, err
		}

		params["parentid"] = taskResult["remotePoolID"].(string)
		lun, err = remoteCli.CreateLun(ctx, params)
		if err != nil {
			log.AddContext(ctx).Errorf("Create remote LUN %s error: %v", lunName, err)
			return nil, err
		}
	}

	return map[string]interface{}{
		"remoteLunID": lun["ID"].(string),
	}, nil
}

func (p *SAN) revertRemoteLun(ctx context.Context, taskResult map[string]interface{}) error {
	lunID, exist := taskResult["remoteLunID"].(string)
	if !exist {
		return nil
	}
	remoteCli := taskResult["remoteCli"].(client.BaseClientInterface)
	return remoteCli.DeleteLun(ctx, lunID)
}

func (p *SAN) createRemoteQoS(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	qos, exist := params["qos"].(map[string]int)
	if !exist {
		return nil, nil
	}

	lunID := taskResult["remoteLunID"].(string)
	remoteCli := taskResult["remoteCli"].(client.BaseClientInterface)

	lun, err := remoteCli.GetLunByID(ctx, lunID)
	if err != nil {
		return nil, err
	}

	qosID, exist := lun["IOCLASSID"].(string)
	if !exist || qosID == "" {
		smartX := smartx.NewSmartX(remoteCli)
		qosID, err = smartX.CreateQos(ctx, lunID, "lun", "", qos)
		if err != nil {
			log.AddContext(ctx).Errorf("Create qos %v for lun %s error: %v", qos, lunID, err)
			return nil, err
		}
	}

	return map[string]interface{}{
		"remoteQosID": qosID,
	}, nil
}

func (p *SAN) revertRemoteQoS(ctx context.Context, taskResult map[string]interface{}) error {
	lunID, lunIDExist := taskResult["remoteLunID"].(string)
	qosID, qosIDExist := taskResult["remoteQosID"].(string)
	if !lunIDExist || !qosIDExist {
		return nil
	}
	remoteCli := taskResult["remoteCli"].(client.BaseClientInterface)
	smartX := smartx.NewSmartX(remoteCli)
	return smartX.DeleteQos(ctx, qosID, lunID, "lun", "")
}

func (p *SAN) createHyperMetro(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	domainID := taskResult["metroDomainID"].(string)
	localLunID := taskResult["localLunID"].(string)
	remoteLunID := taskResult["remoteLunID"].(string)

	pair, err := p.cli.GetHyperMetroPairByLocalObjID(ctx, localLunID)
	if err != nil {
		log.AddContext(ctx).Errorf("Get hypermetro pair by local obj ID %s error: %v", localLunID, err)
		return nil, err
	}

	var pairID string
	if pair == nil {
		_, needFirstSync1 := params["clonefrom"]
		_, needFirstSync2 := params["fromSnapshot"]
		needFirstSync := needFirstSync1 || needFirstSync2
		data := map[string]interface{}{
			"DOMAINID":       domainID,
			"HCRESOURCETYPE": 1,
			"ISFIRSTSYNC":    needFirstSync,
			"LOCALOBJID":     localLunID,
			"REMOTEOBJID":    remoteLunID,
			"SPEED":          4,
		}

		pair, err := p.cli.CreateHyperMetroPair(ctx, data)
		if err != nil {
			log.AddContext(ctx).Errorf("Create hypermetro pair between lun (%s-%s) error: %v",
				localLunID, remoteLunID, err)
			return nil, err
		}

		pairID = pair["ID"].(string)
		if needFirstSync {
			err := p.cli.SyncHyperMetroPair(ctx, pairID)
			if err != nil {
				log.AddContext(ctx).Errorf("Sync hypermetro pair %s error: %v", pairID, err)
				p.cli.DeleteHyperMetroPair(ctx, pairID, true)
				return nil, err
			}
		}
	} else {
		pairID = pair["ID"].(string)
	}

	err = p.waitHyperMetroSyncFinish(ctx, pairID)
	if err != nil {
		log.AddContext(ctx).Errorf("Wait hypermetro pair %s sync done error: %v", pairID, err)
		p.cli.DeleteHyperMetroPair(ctx, pairID, true)
		return nil, err
	}

	return map[string]interface{}{
		"hyperMetroPairID": pairID,
	}, nil
}

func (p *SAN) waitHyperMetroSyncFinish(ctx context.Context, pairID string) error {
	err := utils.WaitUntil(func() (bool, error) {
		pair, err := p.cli.GetHyperMetroPair(ctx, pairID)
		if err != nil {
			return false, err
		}
		if pair == nil {
			msg := fmt.Sprintf("Something wrong with hypermetro pair %s", pairID)
			log.AddContext(ctx).Errorln(msg)
			return false, errors.New(msg)
		}

		healthStatus := pair["HEALTHSTATUS"].(string)
		if healthStatus == hyperMetroPairHealthStatusFault {
			return false, fmt.Errorf("Hypermetro pair %s is fault", pairID)
		}

		runningStatus := pair["RUNNINGSTATUS"].(string)
		if runningStatus == hyperMetroPairRunningStatusToSync ||
			runningStatus == hyperMetroPairRunningStatusSyncing {
			return false, nil
		} else if runningStatus == hyperMetroPairRunningStatusUnknown ||
			runningStatus == hyperMetroPairRunningStatusPause ||
			runningStatus == hyperMetroPairRunningStatusError ||
			runningStatus == hyperMetroPairRunningStatusInvalid {
			return false, fmt.Errorf("Hypermetro pair %s is at running status %s", pairID, runningStatus)
		} else {
			return true, nil
		}
	}, time.Hour*6, time.Second*5)

	if err != nil {
		p.cli.StopHyperMetroPair(ctx, pairID)
		return err
	}

	return nil
}

func (p *SAN) revertHyperMetro(ctx context.Context, taskResult map[string]interface{}) error {
	hyperMetroPairID, exist := taskResult["hyperMetroPairID"].(string)
	if !exist {
		return nil
	}
	err := p.cli.StopHyperMetroPair(ctx, hyperMetroPairID)
	if err != nil {
		log.AddContext(ctx).Warningf("Stop hypermetro pair %s error: %v", hyperMetroPairID, err)
	}
	return p.cli.DeleteHyperMetroPair(ctx, hyperMetroPairID, true)
}

func (p *SAN) getHyperMetroParams(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	metroDomain, exist := params["metrodomain"].(string)
	if !exist || len(metroDomain) == 0 {
		msg := "No hypermetro domain is specified for metro volume"
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	if p.metroRemoteCli == nil {
		msg := "remote client for hypermetro is nil"
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	remotePoolID, err := p.getRemotePoolID(ctx, params, p.metroRemoteCli)
	if err != nil {
		return nil, err
	}

	domain, err := p.metroRemoteCli.GetHyperMetroDomainByName(ctx, metroDomain)
	if err != nil || domain == nil {
		msg := fmt.Sprintf("Cannot get hypermetro domain %s ID", metroDomain)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}
	if status := domain["RUNNINGSTATUS"].(string); status != hyperMetroDomainRunningStatusNormal {
		msg := fmt.Sprintf("Hypermetro domain %s status is not normal", metroDomain)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	return map[string]interface{}{
		"remotePoolID":  remotePoolID,
		"remoteCli":     p.metroRemoteCli,
		"metroDomainID": domain["ID"].(string),
	}, nil
}

func (p *SAN) deleteLocalLunCopy(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunID := params["lunID"].(string)

	lunCopyName, err := p.getLunCopyOfLunID(ctx, lunID)
	if err != nil {
		log.AddContext(ctx).Errorf("Get luncopy of LUN %s error: %v", lunID, err)
		return nil, err
	}

	if lunCopyName != "" {
		err := p.deleteLunCopy(ctx, lunCopyName, true)
		if err != nil {
			log.AddContext(ctx).Errorf("Try to delete luncopy of lun %s error: %v", lunID, err)
			return nil, err
		}
	}

	return nil, nil
}

func (p *SAN) deleteLocalHyperCopy(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunID := params["lunID"].(string)

	// ID of clone pair is the same as destination LUN ID
	clonePair, err := p.cli.GetClonePairInfo(ctx, lunID)
	if err != nil {
		log.AddContext(ctx).Errorf("Get clone pair %s error: %v", lunID, err)
		return nil, err
	}
	if clonePair == nil {
		return nil, nil
	}

	clonePairID := clonePair["ID"].(string)
	err = p.cli.DeleteClonePair(ctx, clonePairID)
	if err != nil {
		log.AddContext(ctx).Errorf("Delete clone pair %s error: %v", clonePairID, err)
		return nil, err
	}

	return nil, nil
}

func (p *SAN) deleteLocalLun(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunName := params["lunName"].(string)
	err := p.deleteLun(ctx, lunName, p.cli)
	return nil, err
}

func (p *SAN) deleteHyperMetroRemoteLun(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	if p.metroRemoteCli == nil {
		log.AddContext(ctx).Warningln("HyperMetro remote cli is nil, the remote lun will be leftover")
		return nil, nil
	}

	lunName := params["lunName"].(string)
	err := p.deleteLun(ctx, lunName, p.metroRemoteCli)
	return nil, err
}

func (p *SAN) deleteHyperMetro(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunID := params["lunID"].(string)

	pair, err := p.cli.GetHyperMetroPairByLocalObjID(ctx, lunID)
	if err != nil {
		log.AddContext(ctx).Errorf("Get hypermetro pair by local obj ID %s error: %v", lunID, err)
		return nil, err
	}
	if pair == nil {
		return nil, nil
	}

	pairID := pair["ID"].(string)
	status := pair["RUNNINGSTATUS"].(string)

	if status == hyperMetroPairRunningStatusNormal ||
		status == hyperMetroPairRunningStatusToSync ||
		status == hyperMetroPairRunningStatusSyncing {
		p.cli.StopHyperMetroPair(ctx, pairID)
	}

	err = p.cli.DeleteHyperMetroPair(ctx, pairID, true)
	if err != nil {
		log.AddContext(ctx).Errorf("Delete hypermetro pair %s error: %v", pairID, err)
		return nil, err
	}

	return nil, nil
}

func (p *SAN) preExpandCheckRemoteCapacity(ctx context.Context,
	params map[string]interface{}, cli client.BaseClientInterface) (string, error) {
	// check the remote pool
	name := params["name"].(string)
	remoteLunName := utils.GetLunName(name)
	remoteLun, err := cli.GetLunByName(ctx, remoteLunName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get lun by name %s error: %v", remoteLunName, err)
		return "", err
	}
	if remoteLun == nil {
		msg := fmt.Sprintf("remote lun %s to extend does not exist", remoteLunName)
		log.AddContext(ctx).Errorln(msg)
		return "", errors.New(msg)
	}

	newSize := params["size"].(int64)
	curSize, err := strconv.ParseInt(remoteLun["CAPACITY"].(string), 10, 64)
	if err != nil {
		return "", err
	}

	if newSize < curSize {
		msg := fmt.Sprintf("Remote Lun %s newSize %d must be greater than curSize %d",
			remoteLunName, newSize, curSize)
		log.AddContext(ctx).Errorln(msg)
		return "", errors.New(msg)
	}

	return remoteLun["ID"].(string), nil
}

func (p *SAN) preExpandHyperMetroCheckRemoteCapacity(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	remoteLunID, err := p.preExpandCheckRemoteCapacity(ctx, params, p.metroRemoteCli)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"remoteLunID": remoteLunID,
	}, nil
}

func (p *SAN) preExpandReplicationCheckRemoteCapacity(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	remoteLunID, err := p.preExpandCheckRemoteCapacity(ctx, params, p.replicaRemoteCli)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"remoteLunID": remoteLunID,
	}, nil
}

func (p *SAN) suspendHyperMetro(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunID := params["lunID"].(string)

	pair, err := p.cli.GetHyperMetroPairByLocalObjID(ctx, lunID)
	if err != nil {
		log.AddContext(ctx).Errorf("Get hypermetro pair by local obj ID %s error: %v", lunID, err)
		return nil, err
	}
	if pair == nil {
		return nil, nil
	}

	pairID := pair["ID"].(string)
	status := pair["RUNNINGSTATUS"].(string)

	if status == hyperMetroPairRunningStatusNormal ||
		status == hyperMetroPairRunningStatusToSync ||
		status == hyperMetroPairRunningStatusSyncing {
		err := p.cli.StopHyperMetroPair(ctx, pairID)
		if err != nil {
			log.AddContext(ctx).Errorf("Suspend san hypermetro pair %s error: %v", pairID, err)
			return nil, err
		}
	}
	return map[string]interface{}{
		"hyperMetroPairID": pairID,
	}, nil
}

func (p *SAN) expandHyperMetroRemoteLun(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	remoteLunID := taskResult["remoteLunID"].(string)
	newSize := params["size"].(int64)

	err := p.metroRemoteCli.ExtendLun(ctx, remoteLunID, newSize)
	if err != nil {
		log.AddContext(ctx).Errorf("Extend hypermetro remote lun %s error: %v", remoteLunID, err)
		return nil, err
	}

	return nil, nil
}

func (p *SAN) syncHyperMetro(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	pairID := taskResult["hyperMetroPairID"].(string)
	if pairID == "" {
		return nil, nil
	}

	err := p.cli.SyncHyperMetroPair(ctx, pairID)
	if err != nil {
		log.AddContext(ctx).Errorf("Sync san hypermetro pair %s error: %v", pairID, err)
		return nil, err
	}

	return nil, nil
}

func (p *SAN) expandLocalLun(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunID := params["lunID"].(string)
	newSize := params["size"].(int64)

	err := p.cli.ExtendLun(ctx, lunID, newSize)
	if err != nil {
		log.AddContext(ctx).Errorf("Expand lun %s error: %v", lunID, err)
		return nil, err
	}

	return nil, nil
}

func (p *SAN) CreateSnapshot(ctx context.Context,
	lunName, snapshotName string) (map[string]interface{}, error) {
	lun, err := p.cli.GetLunByName(ctx, lunName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get lun by name %s error: %v", lunName, err)
		return nil, err
	}
	if lun == nil {
		msg := fmt.Sprintf("Lun %s to create snapshot does not exist", lunName)
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	lunId := lun["ID"].(string)
	snapshot, err := p.cli.GetLunSnapshotByName(ctx, snapshotName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get lun snapshot by name %s error: %v", snapshotName, err)
		return nil, err
	}

	if snapshot != nil {
		snapshotParentId := snapshot["PARENTID"].(string)
		if snapshotParentId != lunId {
			msg := fmt.Sprintf("Snapshot %s is already exist, but the parent LUN %s is incompatible", snapshotName, lunName)
			log.AddContext(ctx).Errorln(msg)
			return nil, errors.New(msg)
		} else {
			snapshotSize, _ := strconv.ParseInt(snapshot["USERCAPACITY"].(string), 10, 64)
			return p.getSnapshotReturnInfo(snapshot, snapshotSize), nil
		}
	}

	taskflow := taskflow.NewTaskFlow(ctx, "Create-LUN-Snapshot")
	taskflow.AddTask("Create-Snapshot", p.createSnapshot, p.revertSnapshot)
	taskflow.AddTask("Active-Snapshot", p.activateSnapshot, nil)

	params := map[string]interface{}{
		"lunID":        lunId,
		"snapshotName": snapshotName,
	}

	result, err := taskflow.Run(params)
	if err != nil {
		taskflow.Revert()
		return nil, err
	}

	snapshot, err = p.cli.GetLunSnapshotByName(ctx, snapshotName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get lun snapshot by name %s error: %v", snapshotName, err)
		return nil, err
	}

	snapshotSize, _ := strconv.ParseInt(result["snapshotSize"].(string), 10, 64)
	return p.getSnapshotReturnInfo(snapshot, snapshotSize), nil
}

func (p *SAN) DeleteSnapshot(ctx context.Context, snapshotName string) error {
	snapshot, err := p.cli.GetLunSnapshotByName(ctx, snapshotName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get lun snapshot by name %s error: %v", snapshotName, err)
		return err
	}

	if snapshot == nil {
		log.AddContext(ctx).Infof("Lun snapshot %s to delete does not exist", snapshotName)
		return nil
	}

	taskflow := taskflow.NewTaskFlow(ctx, "Delete-LUN-Snapshot")
	taskflow.AddTask("Deactivate-Snapshot", p.deactivateSnapshot, nil)
	taskflow.AddTask("Delete-Snapshot", p.deleteSnapshot, nil)

	params := map[string]interface{}{
		"snapshotId": snapshot["ID"].(string),
	}

	_, err = taskflow.Run(params)
	return err
}

func (p *SAN) createSnapshot(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunID := params["lunID"].(string)
	snapshotName := params["snapshotName"].(string)

	snapshot, err := p.cli.CreateLunSnapshot(ctx, snapshotName, lunID)
	if err != nil {
		log.AddContext(ctx).Errorf("Create snapshot %s for lun %s error: %v", snapshotName, lunID, err)
		return nil, err
	}

	err = p.waitSnapshotReady(ctx, snapshotName)
	if err != nil {
		log.AddContext(ctx).Errorf("Wait snapshot ready by name %s error: %v", snapshotName, err)
		return nil, err
	}

	return map[string]interface{}{
		"snapshotId":   snapshot["ID"].(string),
		"snapshotSize": snapshot["USERCAPACITY"].(string),
	}, nil
}

func (p *SAN) waitSnapshotReady(ctx context.Context, snapshotName string) error {
	err := utils.WaitUntil(func() (bool, error) {
		snapshot, err := p.cli.GetLunSnapshotByName(ctx, snapshotName)
		if err != nil {
			return false, err
		}
		if snapshot == nil {
			msg := fmt.Sprintf("Something wrong with snapshot %s", snapshotName)
			log.AddContext(ctx).Errorln(msg)
			return false, errors.New(msg)
		}

		runningStatus := snapshot["RUNNINGSTATUS"].(string)
		if err != nil {
			return false, err
		}

		if runningStatus == snapshotRunningStatusActive ||
			runningStatus == snapshotRunningStatusInactive {
			return true, nil
		} else {
			return false, nil
		}
	}, time.Hour*6, time.Second*5)

	if err != nil {
		return err
	}
	return nil
}

func (p *SAN) revertSnapshot(ctx context.Context, taskResult map[string]interface{}) error {
	snapshotID := taskResult["snapshotId"].(string)
	err := p.cli.DeleteLunSnapshot(ctx, snapshotID)
	if err != nil {
		log.AddContext(ctx).Errorf("Delete snapshot %s error: %v", snapshotID, err)
		return err
	}
	return nil
}

func (p *SAN) activateSnapshot(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	snapshotID := taskResult["snapshotId"].(string)

	err := p.cli.ActivateLunSnapshot(ctx, snapshotID)
	if err != nil {
		log.AddContext(ctx).Errorf("Activate snapshot %s error: %v", snapshotID, err)
		return nil, err
	}
	return nil, nil
}

func (p *SAN) deleteSnapshot(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	snapshotID := params["snapshotId"].(string)

	err := p.cli.DeleteLunSnapshot(ctx, snapshotID)
	if err != nil {
		log.AddContext(ctx).Errorf("Delete snapshot %s error: %v", snapshotID, err)
		return nil, err
	}

	return nil, nil
}

func (p *SAN) deactivateSnapshot(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	snapshotID := params["snapshotId"].(string)

	err := p.cli.DeactivateLunSnapshot(ctx, snapshotID)
	if err != nil {
		log.AddContext(ctx).Errorf("Deactivate snapshot %s error: %v", snapshotID, err)
		return nil, err
	}
	return nil, nil
}

func (p *SAN) getReplicationParams(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	if p.replicaRemoteCli == nil {
		msg := "remote client for replication is nil"
		log.AddContext(ctx).Errorln(msg)
		return nil, errors.New(msg)
	}

	remotePoolID, err := p.getRemotePoolID(ctx, params, p.replicaRemoteCli)
	if err != nil {
		return nil, err
	}

	remoteSystem, err := p.replicaRemoteCli.GetSystem(ctx)
	if err != nil {
		log.AddContext(ctx).Errorf("Remote device is abnormal: %v", err)
		return nil, err
	}

	sn := remoteSystem["ID"].(string)
	remoteDeviceID, err := p.getRemoteDeviceID(ctx, sn)
	if err != nil {
		return nil, err
	}

	res := map[string]interface{}{
		"remotePoolID":   remotePoolID,
		"remoteCli":      p.replicaRemoteCli,
		"remoteDeviceID": remoteDeviceID,
		"resType":        11,
	}

	return res, nil
}

func (p *SAN) deleteReplicationPair(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunID := params["lunID"].(string)

	pairs, err := p.cli.GetReplicationPairByResID(ctx, lunID, 11)
	if err != nil {
		return nil, err
	}

	if pairs == nil || len(pairs) == 0 {
		return nil, nil
	}

	for _, pair := range pairs {
		pairID := pair["ID"].(string)

		runningStatus := pair["RUNNINGSTATUS"].(string)
		if runningStatus == replicationPairRunningStatusNormal ||
			runningStatus == replicationPairRunningStatusSync {
			p.cli.SplitReplicationPair(ctx, pairID)
		}

		err = p.cli.DeleteReplicationPair(ctx, pairID)
		if err != nil {
			log.AddContext(ctx).Errorf("Delete replication pair %s error: %v", pairID, err)
			return nil, err
		}
	}

	return nil, nil
}

func (p *SAN) deleteReplicationRemoteLun(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	if p.replicaRemoteCli == nil {
		log.AddContext(ctx).Warningln("Replication remote cli is nil, the remote lun will be leftover")
		return nil, nil
	}

	lunName := params["lunName"].(string)
	err := p.deleteLun(ctx, lunName, p.replicaRemoteCli)
	return nil, err
}

func (p *SAN) deleteLun(ctx context.Context, name string, cli client.BaseClientInterface) error {
	lun, err := cli.GetLunByName(ctx, name)
	if err != nil {
		log.AddContext(ctx).Errorf("Get lun by name %s error: %v", name, err)
		return err
	}
	if lun == nil {
		log.AddContext(ctx).Infof("Lun %s to delete does not exist", name)
		return nil
	}

	lunID := lun["ID"].(string)

	qosID, exist := lun["IOCLASSID"].(string)
	if exist && qosID != "" {
		smartX := smartx.NewSmartX(cli)
		err := smartX.DeleteQos(ctx, qosID, lunID, "lun", "")
		if err != nil {
			log.AddContext(ctx).Errorf("Remove lun %s from qos %s error: %v", lunID, qosID, err)
			return err
		}
	}

	err = cli.DeleteLun(ctx, lunID)
	if err != nil {
		log.AddContext(ctx).Errorf("Delete lun %s error: %v", lunID, err)
		return err
	}

	return nil
}

func (p *SAN) splitReplication(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	lunID := params["lunID"].(string)

	pairs, err := p.cli.GetReplicationPairByResID(ctx, lunID, 11)
	if err != nil {
		return nil, err
	}

	if pairs == nil || len(pairs) == 0 {
		return nil, nil
	}

	replicationPairIDs := []string{}

	for _, pair := range pairs {
		pairID := pair["ID"].(string)

		runningStatus := pair["RUNNINGSTATUS"].(string)
		if runningStatus != replicationPairRunningStatusNormal &&
			runningStatus != replicationPairRunningStatusSync {
			continue
		}

		err := p.cli.SplitReplicationPair(ctx, pairID)
		if err != nil {
			return nil, err
		}

		replicationPairIDs = append(replicationPairIDs, pairID)
	}

	return map[string]interface{}{
		"replicationPairIDs": replicationPairIDs,
	}, nil
}

func (p *SAN) expandReplicationRemoteLun(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	remoteLunID := taskResult["remoteLunID"].(string)
	newSize := params["size"].(int64)

	err := p.replicaRemoteCli.ExtendLun(ctx, remoteLunID, newSize)
	if err != nil {
		log.AddContext(ctx).Errorf("Extend replication remote lun %s error: %v", remoteLunID, err)
		return nil, err
	}

	return nil, nil
}

func (p *SAN) syncReplication(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	replicationPairIDs := taskResult["replicationPairIDs"].([]string)

	for _, pairID := range replicationPairIDs {
		err := p.cli.SyncReplicationPair(ctx, pairID)
		if err != nil {
			log.AddContext(ctx).Errorf("Sync san replication pair %s error: %v", pairID, err)
			return nil, err
		}
	}

	return nil, nil
}
