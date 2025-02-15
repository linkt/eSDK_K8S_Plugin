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
	"errors"
	"fmt"
	"strconv"

	"huawei-csi-driver/storage/oceanstor/client"
	"huawei-csi-driver/storage/oceanstor/smartx"
	"huawei-csi-driver/utils"
	"huawei-csi-driver/utils/log"
)

type Base struct {
	cli              client.BaseClientInterface
	metroRemoteCli   client.BaseClientInterface
	replicaRemoteCli client.BaseClientInterface
	product          string
}

func (p *Base) commonPreCreate(ctx context.Context, params map[string]interface{}) error {
	analyzers := [...]func(context.Context, map[string]interface{}) error{
		p.getAllocType,
		p.getCloneSpeed,
		p.getPoolID,
		p.getQoS,
	}

	for _, analyzer := range analyzers {
		err := analyzer(ctx, params)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *Base) getAllocType(_ context.Context, params map[string]interface{}) error {
	if v, exist := params["alloctype"].(string); exist && v == "thick" {
		params["alloctype"] = 0
	} else {
		params["alloctype"] = 1
	}

	return nil
}

func (p *Base) getCloneSpeed(_ context.Context, params map[string]interface{}) error {
	_, cloneExist := params["clonefrom"].(string)
	_, srcVolumeExist := params["sourcevolumename"].(string)
	_, srcSnapshotExist := params["sourcesnapshotname"].(string)
	if !(cloneExist || srcVolumeExist || srcSnapshotExist) {
		return nil
	}

	if v, exist := params["clonespeed"].(string); exist && v != "" {
		speed, err := strconv.Atoi(v)
		if err != nil || speed < 1 || speed > 4 {
			return fmt.Errorf("error config %s for clonespeed", v)
		}
		params["clonespeed"] = speed
	} else {
		params["clonespeed"] = 3
	}

	return nil
}

func (p *Base) getPoolID(ctx context.Context, params map[string]interface{}) error {
	poolName, exist := params["storagepool"].(string)
	if !exist || poolName == "" {
		return errors.New("must specify storage pool to create volume")
	}

	pool, err := p.cli.GetPoolByName(ctx, poolName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get storage pool %s info error: %v", poolName, err)
		return err
	}
	if pool == nil {
		return fmt.Errorf("storage pool %s doesn't exist", poolName)
	}

	params["poolID"] = pool["ID"].(string)

	return nil
}

func (p *Base) getQoS(ctx context.Context, params map[string]interface{}) error {
	if v, exist := params["qos"].(string); exist && v != "" {
		qos, err := smartx.ExtractQoSParameters(ctx, p.product, v)
		if err != nil {
			return utils.Errorf(ctx, "qos parameter %s error: %v", v, err)
		}

		validatedQos, err := smartx.ValidateQoSParameters(p.product, qos)
		if err != nil {
			return utils.Errorf(ctx, "validate qos parameters failed, error %v", err)
		}
		params["qos"] = validatedQos
	}

	return nil
}

func (p *Base) getRemotePoolID(ctx context.Context,
	params map[string]interface{}, remoteCli client.BaseClientInterface) (string, error) {
	remotePool, exist := params["remotestoragepool"].(string)
	if !exist || len(remotePool) == 0 {
		msg := "no remote pool is specified"
		log.AddContext(ctx).Errorln(msg)
		return "", errors.New(msg)
	}

	pool, err := remoteCli.GetPoolByName(ctx, remotePool)
	if err != nil {
		log.AddContext(ctx).Errorf("Get remote storage pool %s info error: %v", remotePool, err)
		return "", err
	}
	if pool == nil {
		return "", fmt.Errorf("remote storage pool %s doesn't exist", remotePool)
	}

	return pool["ID"].(string), nil
}

func (p *Base) preExpandCheckCapacity(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	// check the local pool
	localParentName := params["localParentName"].(string)
	pool, err := p.cli.GetPoolByName(ctx, localParentName)
	if err != nil || pool == nil {
		msg := fmt.Sprintf("Get storage pool %s info error: %v", localParentName, err)
		log.AddContext(ctx).Errorf(msg)
		return nil, errors.New(msg)
	}

	return nil, nil
}

func (p *Base) getSnapshotReturnInfo(snapshot map[string]interface{}, snapshotSize int64) map[string]interface{} {
	snapshotCreated, _ := strconv.ParseInt(snapshot["TIMESTAMP"].(string), 10, 64)
	snapshotSizeBytes := snapshotSize * 512
	return map[string]interface{}{
		"CreationTime": snapshotCreated,
		"SizeBytes":    snapshotSizeBytes,
		"ParentID":     snapshot["PARENTID"].(string),
	}
}

func (p *Base) createReplicationPair(ctx context.Context,
	params, taskResult map[string]interface{}) (map[string]interface{}, error) {
	resType := taskResult["resType"].(int)
	remoteDeviceID := taskResult["remoteDeviceID"].(string)

	var localID string
	var remoteID string

	if resType == 11 {
		localID = taskResult["localLunID"].(string)
		remoteID = taskResult["remoteLunID"].(string)
	} else {
		localID = taskResult["localFSID"].(string)
		remoteID = taskResult["remoteFSID"].(string)
	}

	data := map[string]interface{}{
		"LOCALRESID":       localID,
		"LOCALRESTYPE":     resType,
		"REMOTEDEVICEID":   remoteDeviceID,
		"REMOTERESID":      remoteID,
		"REPLICATIONMODEL": 2, // asynchronous replication
		"SYNCHRONIZETYPE":  2, // timed wait after synchronization begins
		"SPEED":            4, // highest speed
	}

	replicationSyncPeriod, exist := params["replicationSyncPeriod"]
	if exist {
		data["TIMINGVAL"] = replicationSyncPeriod
	}

	vStorePairID, exist := taskResult["vStorePairID"]
	if exist {
		data["VSTOREPAIRID"] = vStorePairID
	}

	pair, err := p.cli.CreateReplicationPair(ctx, data)
	if err != nil {
		log.AddContext(ctx).Errorf("Create replication pair error: %v", err)
		return nil, err
	}

	pairID := pair["ID"].(string)
	err = p.cli.SyncReplicationPair(ctx, pairID)
	if err != nil {
		log.AddContext(ctx).Errorf("Sync replication pair %s error: %v", pairID, err)
		p.cli.DeleteReplicationPair(ctx, pairID)
		return nil, err
	}

	return nil, nil
}

func (p *Base) getRemoteDeviceID(ctx context.Context, deviceSN string) (string, error) {
	remoteDevice, err := p.cli.GetRemoteDeviceBySN(ctx, deviceSN)
	if err != nil {
		log.AddContext(ctx).Errorf("Get remote device %s error: %v", deviceSN, err)
		return "", err
	}
	if remoteDevice == nil {
		msg := fmt.Sprintf("Remote device of SN %s does not exist", deviceSN)
		log.AddContext(ctx).Errorln(msg)
		return "", errors.New(msg)
	}

	if remoteDevice["HEALTHSTATUS"] != remoteDeviceHealthStatus ||
		remoteDevice["RUNNINGSTATUS"] != remoteDeviceRunningStatusLinkUp {
		msg := fmt.Sprintf("Remote device %s status is not normal", deviceSN)
		log.AddContext(ctx).Errorln(msg)
		return "", errors.New(msg)
	}

	return remoteDevice["ID"].(string), nil
}

func (p *Base) getWorkLoadIDByName(ctx context.Context,
	cli client.BaseClientInterface,
	workloadTypeName string) (string, error) {
	workloadTypeID, err := cli.GetApplicationTypeByName(ctx, workloadTypeName)
	if err != nil {
		log.AddContext(ctx).Errorf("Get application types returned error: %v", err)
		return "", err
	}
	if workloadTypeID == "" {
		msg := fmt.Sprintf("The workloadType %s does not exist on storage", workloadTypeName)
		log.AddContext(ctx).Errorln(msg)
		return "", errors.New(msg)
	}
	return workloadTypeID, nil
}

func (p *Base) setWorkLoadID(ctx context.Context, cli client.BaseClientInterface, params map[string]interface{}) error {
	if val, ok := params["applicationtype"].(string); ok {
		workloadTypeID, err := p.getWorkLoadIDByName(ctx, cli, val)
		if err != nil {
			return err
		}
		params["workloadTypeID"] = workloadTypeID
	}
	return nil
}

func (p *Base) prepareVolObj(ctx context.Context, params, res map[string]interface{}) utils.Volume {
	volName, isStr := params["name"].(string)
	if !isStr {
		// Not expecting this error to happen
		log.AddContext(ctx).Warningf("Expecting string for volume name, received type %T", params["name"])
	}
	volObj := utils.NewVolume(volName)
	if res != nil {
		if lunWWN, ok := res["lunWWN"].(string); ok {
			volObj.SetLunWWN(lunWWN)
		}
	}
	return volObj
}
