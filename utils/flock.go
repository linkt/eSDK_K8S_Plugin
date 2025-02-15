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

package utils

import (
	"os"
	"syscall"
)

type Flock struct {
	name string
	f    *os.File
}

func NewFlock(file string) *Flock {
	return &Flock{
		name: file,
	}
}

func (p *Flock) Lock() error {
	f, err := os.OpenFile(p.name, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}

	p.f = f

	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func (p *Flock) UnLock() {
	defer p.f.Close()
	syscall.Flock(int(p.f.Fd()), syscall.LOCK_UN)
}
