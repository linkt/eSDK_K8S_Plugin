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

package connector

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/prashantv/gostub"
	"github.com/stretchr/testify/assert"

	"huawei-csi-driver/utils"
)

func TestGetDevice(t *testing.T) {
	const (
		hasPrefixDM       = "/test/../../dm-test"
		hasPrefixNVMe     = "/test/../../nvme-test"
		hasPrefixSD       = "/test/../../sd-test"
		invalidDeviceLink = "../../"
		emptyDeviceLink   = ""
	)
	type args struct {
		findDeviceMap map[string]string
		deviceLink    string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{"HasPrefixDM", args{map[string]string{}, hasPrefixDM}, "dm-test"},
		{"HasPrefixNVMe", args{map[string]string{}, hasPrefixNVMe}, "nvme-test"},
		{"HasPrefixSD", args{map[string]string{}, hasPrefixSD}, "sd-test"},
		{"DeviceLinkIsEmpty", args{map[string]string{}, emptyDeviceLink}, ""},
		{"TheSplitLengthIsLessThenTwo", args{map[string]string{}, invalidDeviceLink}, ""},
		{"HasPrefixNvmeButExist", args{map[string]string{"nvme-test": "test"}, hasPrefixNVMe}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getDevice(tt.args.findDeviceMap, tt.args.deviceLink); got != tt.want {
				t.Errorf("getDevice() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetDeviceLink(t *testing.T) {
	const (
		stubTgtLunGUID = "test123456"

		normalCmdOutput     = "test output"
		nofileCmdOutput     = "No such file or directory"
		emptyCmdOutput      = ""
		otherErrorCmdOutput = "other result"
	)

	var stubCtx = context.TODO()

	type args struct {
		ctx        context.Context
		tgtLunGUID string
	}
	type outputs struct {
		output string
		err    error
	}
	tests := []struct {
		name    string
		args    args
		outputs outputs
		want    string
		wantErr bool
	}{
		// TODO: Add test cases.
		{"Normal", args{stubCtx, stubTgtLunGUID}, outputs{normalCmdOutput, nil}, "test output", false},
		{"EmptyCmdResult", args{stubCtx, stubTgtLunGUID}, outputs{emptyCmdOutput, errors.New("test")}, "", false},
		{"CmdResultIsFileOrDirectoryNoExist", args{stubCtx, stubTgtLunGUID}, outputs{nofileCmdOutput, errors.New("test")}, "", false},
		{"CmdResultIsOtherError", args{stubCtx, stubTgtLunGUID}, outputs{otherErrorCmdOutput, errors.New("test")}, "", true},
	}

	stub := utils.ExecShellCmd
	defer func() {
		utils.ExecShellCmd = stub
	}()
	for _, tt := range tests {
		utils.ExecShellCmd = func(_ context.Context, format string, args ...interface{}) (string, error) {
			return tt.outputs.output, tt.outputs.err
		}
		t.Run(tt.name, func(t *testing.T) {
			got, err := getDeviceLink(tt.args.ctx, tt.args.tgtLunGUID)
			if (err != nil) != tt.wantErr {
				t.Errorf("getDeviceLink() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("getDeviceLink() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsUltraPathDevice(t *testing.T) {
	const device = "dm-test"
	var stubCtx = context.TODO()

	type args struct {
		ctx    context.Context
		device string
	}
	type outputs struct {
		output string
		err    error
	}
	tests := []struct {
		name    string
		args    args
		outputs outputs
		want    bool
	}{
		{"Normal", args{stubCtx, device}, outputs{"test output dm-test", nil}, true},
		{"CmdError", args{stubCtx, device}, outputs{"test output", errors.New("test")}, false},
	}

	stub := utils.ExecShellCmd
	defer func() {
		utils.ExecShellCmd = stub
	}()
	for _, tt := range tests {
		utils.ExecShellCmd = func(_ context.Context, format string, args ...interface{}) (string, error) {
			return tt.outputs.output, tt.outputs.err
		}

		t.Run(tt.name, func(t *testing.T) {
			if got := isUltraPathDevice(tt.args.ctx, tt.args.device); got != tt.want {
				t.Errorf("isUltraPathDevice() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetDevices(t *testing.T) {
	const (
		normalDeviceLink  = "/test/../../dm-test\n/test/../../sd-test"
		emptyDeviceLink   = ""
		invalidDeviceLink = "/test/../../"
	)
	var emptyDevices []string

	type args struct {
		deviceLink string
	}
	tests := []struct {
		name string
		args args
		want []string
	}{
		{"Normal", args{normalDeviceLink}, []string{"dm-test", "sd-test"}},
		{"EmptyDeviceLink", args{emptyDeviceLink}, emptyDevices},
		{"TheSplitLengthLessThenTwo", args{invalidDeviceLink}, emptyDevices},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getDevices(tt.args.deviceLink); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getDevices() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckConnectSuccess(t *testing.T) {
	type args struct {
		ctx       context.Context
		device    string
		tgtLunWWN string
	}

	tests := []struct {
		name string
		args args
		want bool
	}{
		{"Normal", args{context.TODO(), "normal-device", "normal-lunWWN"}, true},
		{"DeviceIsNotReadable", args{context.TODO(), "test-device", "normal-lunWWN"}, false},
		{"DeviceIsNotAvailable", args{context.TODO(), "normal-device", "test-tgtLunWWN"}, false},
	}

	stubIsDeviceReadable := IsDeviceReadable
	defer func() {
		IsDeviceReadable = stubIsDeviceReadable
	}()

	stubIsDeviceAvailable := IsDeviceAvailable
	defer func() {
		IsDeviceAvailable = stubIsDeviceAvailable
	}()
	for _, tt := range tests {
		IsDeviceReadable = func(_ context.Context, devicePath string) (bool, error) {
			if devicePath != "/dev/normal-device" {
				return false, errors.New("test")
			}

			return true, nil
		}
		IsDeviceAvailable = func(_ context.Context, device, lunWWN string) (bool, error) {
			if lunWWN != "normal-lunWWN" {
				return false, errors.New("test")
			}

			return true, nil
		}
		t.Run(tt.name, func(t *testing.T) {
			if got := CheckConnectSuccess(tt.args.ctx, tt.args.device, tt.args.tgtLunWWN); got != tt.want {
				t.Errorf("CheckConnectSuccess() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDeviceAvailable(t *testing.T) {
	type args struct {
		ctx    context.Context
		device string
		lunWWN string
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{"SCSIWwn", args{context.TODO(), "/dev/dm-device", "dm-device"}, true, false},
		{"NVMeWwn", args{context.TODO(), "/dev/nvme-device", "nvme-device"}, true, false},
		{"CanontGetWwn", args{context.TODO(), "/dev/other-device", "test lunWWN"}, false, true},
	}

	var stubGetSCSIWwn = GetSCSIWwn
	defer func() {
		GetSCSIWwn = stubGetSCSIWwn
	}()

	var stubGetNVMeWwn = GetNVMeWwn
	defer func() {
		GetNVMeWwn = stubGetNVMeWwn
	}()
	for _, tt := range tests {
		GetSCSIWwn = func(_ context.Context, hostDevice string) (string, error) {
			if hostDevice == "/dev/dm-device" {
				return hostDevice, nil
			}
			return "", errors.New("test error")
		}

		GetNVMeWwn = func(_ context.Context, device string) (string, error) {
			return device, nil
		}

		t.Run(tt.name, func(t *testing.T) {
			got, err := IsDeviceAvailable(tt.args.ctx, tt.args.device, tt.args.lunWWN)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsDeviceAvailable() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("IsDeviceAvailable() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDeviceReadable(t *testing.T) {
	type args struct {
		ctx        context.Context
		devicePath string
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{"CanReadDevice", args{context.TODO(), "Normal"}, true, false},
		{"CanNotReadDevice", args{context.TODO(), "UnNormal"}, false, true},
	}

	stub := ReadDevice
	defer func() {
		ReadDevice = stub
	}()
	for _, tt := range tests {
		ReadDevice = func(_ context.Context, dev string) ([]byte, error) {
			if dev != "Normal" {
				return []byte{}, errors.New("test error")
			}
			return []byte(dev), nil
		}
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsDeviceReadable(tt.args.ctx, tt.args.devicePath)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsDeviceReadable() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("IsDeviceReadable() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestXfsResize(t *testing.T) {
	type args struct {
		ctx        context.Context
		devicePath string
	}
	type outputs struct {
		output string
		err    error
	}
	tests := []struct {
		name    string
		args    args
		outputs outputs
		wantErr bool
	}{
		{"Normal", args{context.TODO(), "device path"}, outputs{"normal cmd output", nil}, false},
		{"ErrorOutput", args{context.TODO(), "device path"}, outputs{"unnormal cmd output", errors.New("test error")}, true},
	}

	stub := utils.ExecShellCmd
	defer func() {
		utils.ExecShellCmd = stub
	}()
	for _, tt := range tests {
		utils.ExecShellCmd = func(_ context.Context, format string, args ...interface{}) (string, error) {
			return tt.outputs.output, tt.outputs.err
		}
		t.Run(tt.name, func(t *testing.T) {
			if err := xfsResize(tt.args.ctx, tt.args.devicePath); (err != nil) != tt.wantErr {
				t.Errorf("xfsResize() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetVirtualDevice(t *testing.T) {
	type args struct {
		ctx    context.Context
		LunWWN string
	}
	type outputs struct {
		output    []string
		cmdOutput string
		err       error
		cmdErr    error
	}
	tests := []struct {
		name           string
		args           args
		mockOutputs    outputs
		wantDeviceName string
		wantDeviceKind int
		wantErr        bool
	}{
		{"NormalUltrapath*", args{context.TODO(), "7100e98b8e19b76d00e4069a00000003"}, outputs{[]string{"ultrapathh"}, "", nil, nil}, "ultrapathh", UseUltraPathNVMe, false},
		{"NormalDm-*", args{context.TODO(), "7100e98b8e19b76d00e4069a00000003"}, outputs{[]string{"dm-2"}, "lrwxrwxrwx. 1 root root       7 Mar 14 10:26 mpatha -> ../dm-2", nil, nil}, "dm-2", UseDMMultipath, false},
		{"NormalPhysicalSd*", args{context.TODO(), "7100e98b8e19b76d00e4069a00000003"}, outputs{[]string{"sdd"}, "", nil, nil}, "sdd", NotUseMultipath, false},
		{"NormalPhysicalNVMe*", args{context.TODO(), "7100e98b8e19b76d00e4069a00000003"}, outputs{[]string{"nvme1n1"}, "", nil, nil}, "nvme1n1", NotUseMultipath, false},
		{"ErrorMultiUltrapath*", args{context.TODO(), "7100e98b8e19b76d00e4069a00000003"}, outputs{[]string{"ultrapathh", "ultrapathi"}, "", nil, nil}, "", 0, true},
		{"ErrorPartitionUltrapath*", args{context.TODO(), "7100e98b8e19b76d00e4069a00000003"}, outputs{[]string{"ultrapathh", "ultrapathh2"}, "", nil, nil}, "", 0, true},
		{"ErrorPartitionDm-*", args{context.TODO(), "7100e98b8e19b76d00e4069a00000003"}, outputs{[]string{"dm-2"}, "lrwxrwxrwx. 1 root root       7 Mar 14 10:26 mpatha2 -> ../dm-2", nil, nil}, "", 0, true},
		{"ErrorPartitionNvme*", args{context.TODO(), "7100e98b8e19b76d00e4069a00000003"}, outputs{[]string{"nvme1n1", "nvme1n1p1"}, "", nil, nil}, "", 0, true},
	}

	stub := GetDevicesByGUID
	stub2 := utils.ExecShellCmd
	defer func() {
		GetDevicesByGUID = stub
		utils.ExecShellCmd = stub2
	}()

	for _, tt := range tests {
		GetDevicesByGUID = func(_ context.Context, tgtLunGUID string) ([]string, error) {
			return tt.mockOutputs.output, tt.mockOutputs.err
		}
		utils.ExecShellCmd = func(ctx context.Context, format string, args ...interface{}) (string, error) {
			return tt.mockOutputs.cmdOutput, tt.mockOutputs.cmdErr
		}

		t.Run(tt.name, func(t *testing.T) {
			dev, kind, err := GetVirtualDevice(tt.args.ctx, tt.args.LunWWN)
			if (err != nil) != tt.wantErr || dev != tt.wantDeviceName || kind != tt.wantDeviceKind {
				t.Errorf("GetVirtualDevice() error = %v, wantErr %v; dev: %s, want: %s; kind: %d, want: %d",
					err, tt.wantErr, dev, tt.wantDeviceName, kind, tt.wantDeviceKind)
			}
		})
	}
}

func TestWatchDMDevice(t *testing.T) {
	var cases = []struct {
		name             string
		lunWWN           string
		lunName          string
		expectPathNumber int
		devices          []string
		aggregatedTime   time.Duration
		pathCompleteTime time.Duration
		err              error
	}{
		{
			"Normal",
			"6582575100bc510f12345678000103e8",
			"dm-0",
			3,
			[]string{"sdb", "sdc", "sdd"},
			100 * time.Millisecond,
			100 * time.Millisecond,
			nil,
		},
		{
			"PathIncomplete",
			"6582575100bc510f12345678000103e8",
			"dm-0",
			3,
			[]string{"sdb", "sdc"},
			100 * time.Millisecond,
			100 * time.Millisecond,
			errors.New(VolumePathIncomplete),
		},
		{
			"Timeout",
			"6582575100bc510f12345678000103e8",
			"dm-0",
			3,
			[]string{"sdb", "sdc", "sdd"},
			100 * time.Millisecond,
			200 * time.Millisecond,
			errors.New(VolumeNotFound),
		},
	}

	stubs := gostub.Stub(&ScanVolumeTimeout, 10*time.Millisecond)
	defer stubs.Reset()

	for _, c := range cases {
		var startTime = time.Now()

		stubs.Stub(&utils.ExecShellCmd, func(ctx context.Context, format string, args ...interface{}) (string, error) {
			if time.Now().Sub(startTime) > c.aggregatedTime {
				return fmt.Sprintf("name    sysfs uuid                             \nmpathja %s  %s", c.lunName, c.lunWWN), nil
			} else {
				return "", errors.New("err")
			}
		})

		stubs.Stub(&getDeviceFromDM, func(dm string) ([]string, error) {
			if time.Now().Sub(startTime) > c.pathCompleteTime {
				return c.devices, nil
			} else {
				return nil, errors.New(VolumeNotFound)
			}
		})

		_, err := WatchDMDevice(context.TODO(), c.lunWWN, c.expectPathNumber)
		assert.Equal(t, c.err, err, "%s, err:%v", c.name, err)
	}
}

func TestGetFsTypeByDevPath(t *testing.T) {
	type args struct {
		ctx     context.Context
		devPath string
	}
	type outputs struct {
		output string
		err    error
	}

	tests := []struct {
		name       string
		args       args
		mockOutput outputs
		want       string
		wantErr    bool
	}{
		{"Normal", args{context.TODO(), "/dev/dm-2"}, outputs{"xfs\n", nil}, "xfs", false},
		{"RunCommandError", args{context.TODO(), "/dev/dm-3"}, outputs{"", errors.New("mock error")}, "", true},
	}

	stub := utils.ExecShellCmd
	defer func() {
		utils.ExecShellCmd = stub
	}()

	for _, tt := range tests {
		utils.ExecShellCmd = func(ctx context.Context, format string, args ...interface{}) (string, error) {
			return tt.mockOutput.output, tt.mockOutput.err
		}

		t.Run(tt.name, func(t *testing.T) {
			fsType, err := GetFsTypeByDevPath(tt.args.ctx, tt.args.devPath)
			if (err != nil) != tt.wantErr || fsType != tt.want {
				t.Errorf("Test GetFsTypeByDevPath() error = %v, wantErr: [%v]; fsType: [%s], want: [%s]", err, tt.wantErr, fsType, tt.want)
			}
		})
	}
}
