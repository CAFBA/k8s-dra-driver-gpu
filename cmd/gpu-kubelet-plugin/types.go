/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

// 设备类型常量定义，这些常量用于在Device对象的"type"属性中标识设备类型
// 调度器和用户可以通过类型来选择和过滤设备
const (
	// GpuDeviceType 表示完整的物理GPU设备
	GpuDeviceType = "gpu"

	// MigDeviceType 表示MIG（Multi-Instance GPU）设备
	// MIG允许将一个物理GPU分割成多个独立的GPU实例
	// 每个MIG实例有自己的内存分区和计算资源，提供硬件级别的隔离
	MigDeviceType = "mig"

	// VfioDeviceType 表示VFIO（Virtual Function I/O）设备
	// VFIO是Linux内核框架，允许安全地将设备直通给用户空间或虚拟机
	// 对于GPU，这意味着可以将整个GPU或GPU的虚拟功能分配给虚拟机
	VfioDeviceType = "vfio"

	// UnknownDeviceType 表示未知或无法识别的设备类型
	// 这是一个后备值，用于错误处理和调试
	// 正常情况下不应该出现，如果出现通常表示设备枚举或识别过程中的问题
	UnknownDeviceType = "unknown"
)

// UUIDProvider 定义获取设备UUID的接口
// 这个接口抽象设备UUID的获取方式，使得不同的设备集合类型可以统一处理
type UUIDProvider interface {
	// UUIDs 返回所有设备的UUID列表，包括GPU、MIG设备和其他类型设备的UUID
	// 1. 获取节点上所有可用设备的完整列表
	// 2. 与ResourceClaim中请求的UUID进行匹配
	// 3. 生成设备清单用于状态同步
	UUIDs() []string

	// GpuUUIDs 返回所有完整GPU设备的UUID列表，只包含物理GPU（GpuDeviceType），不包括MIG设备
	// 1. 当用户明确请求完整GPU时进行设备筛选
	// 2. 区分完整GPU和MIG设备的资源池
	// 3. 在MIG禁用的场景下获取可用GPU列表
	GpuUUIDs() []string

	// MigDeviceUUIDs 返回所有MIG设备的UUID列表，只包含MIG实例（MigDeviceType），不包括物理GPU
	// 1. 当用户明确请求MIG设备时进行设备筛选
	// 2. 在MIG启用的场景下获取可用MIG实例列表
	// 3. 管理和跟踪MIG设备的生命周期
	MigDeviceUUIDs() []string
}
