/*
 * Copyright (c) 2022-2025, NVIDIA CORPORATION.  All rights reserved.
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

import (
	"slices"

	// resourceapi是Kubernetes DRA的资源API
	// 用于定义Device类型，这是DRA框架中设备的标准表示
	resourceapi "k8s.io/api/resource/v1"
)

// AllocatableDevice 表示一个可分配的设备
// 这是一个联合类型（union type），同一时间只能有一个字段非nil
// 设计原因：
// 1. 统一处理不同类型的设备（GPU、MIG、VFIO）
// 2. 类型安全：通过方法确保正确访问设备信息
// 3. 可扩展：未来添加新设备类型只需添加新字段
type AllocatableDevice struct {
	// Gpu 指向完整的物理GPU设备信息
	// 当此字段非nil时，表示这是一个完整GPU设备
	Gpu *GpuInfo

	// Mig 指向MIG设备信息
	// 当此字段非nil时，表示这是一个MIG实例
	Mig *MigDeviceInfo

	// Vfio 指向VFIO设备信息
	// 当此字段非nil时，表示这是一个VFIO直通设备
	Vfio *VfioDeviceInfo
}

// Type 返回设备的类型字符串
// 通过检查哪个字段非nil来确定设备类型
// 这个方法用于：
// 1. 在日志中标识设备类型
// 2. 在switch语句中进行类型分支
// 3. 设置Device对象的type属性
func (d AllocatableDevice) Type() string {
	// 检查顺序很重要：GPU -> MIG -> VFIO -> Unknown
	// 确保优先识别最常见的类型
	if d.Gpu != nil {
		return GpuDeviceType
	}
	if d.Mig != nil {
		return MigDeviceType
	}
	if d.Vfio != nil {
		return VfioDeviceType
	}
	// 如果所有字段都是nil，返回unknown
	// 这通常表示编程错误或未初始化的设备
	return UnknownDeviceType
}

// CanonicalName 返回设备的规范名称
// 根据设备类型调用相应的CanonicalName方法
// 规范名称用于：
// 1. 在AllocatableDevices map中作为key
// 2. 在ResourceSlice中唯一标识设备
// 3. 在日志和错误消息中引用设备
func (d *AllocatableDevice) CanonicalName() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.CanonicalName()
	case MigDeviceType:
		return d.Mig.CanonicalName()
	case VfioDeviceType:
		return d.Vfio.CanonicalName()
	}
	// 如果到达这里，说明Type()返回了UnknownDeviceType
	// 这是一个严重的编程错误，应该立即失败而不是返回错误
	panic("unexpected type for AllocatableDevice")
}

// GetDevice 将AllocatableDevice转换为Kubernetes资源API的Device对象
// 这是DRA框架与设备信息的桥梁
// 根据设备类型调用相应的GetDevice方法
// 返回的Device对象包含：
// 1. 设备名称（Name）
// 2. 设备属性（Attributes）：用于设备选择器匹配
// 3. 设备容量（Capacity）：用于资源配额和调度
func (d *AllocatableDevice) GetDevice() resourceapi.Device {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.GetDevice()
	case MigDeviceType:
		return d.Mig.GetDevice()
	case VfioDeviceType:
		return d.Vfio.GetDevice()
	}
	// 如果到达这里，说明Type()返回了UnknownDeviceType
	// panic而不是返回零值，因为这表示严重的逻辑错误
	panic("unexpected type for AllocatableDevice")
}

// This concept will have to change for abstract allocatable devices that do not
// have a UUID before actualization.
// UUID 返回设备的唯一标识符
// 注意：这个概念可能需要改变，以支持在实例化之前没有UUID的抽象可分配设备
//
// 为什么每个设备都需要UUID：
// 1. 跨重启持久化标识：UUID在系统重启后保持不变
// 2. 精确设备匹配：用户可以指定特定UUID的设备
// 3. 状态跟踪：追踪设备的分配历史和状态
func (d AllocatableDevice) UUID() string {
	if d.Gpu != nil {
		return d.Gpu.UUID
	}
	if d.Mig != nil {
		return d.Mig.UUID
	}
	if d.Vfio != nil {
		return d.Vfio.UUID
	}
	// 如果所有字段都是nil，这是一个严重错误
	// panic确保问题立即被发现
	panic("unexpected type for AllocatableDevice")
}

// AllocatableDeviceList 是AllocatableDevice指针的切片
// 用于表示设备列表，如：
// 1. 同一GPU上的所有MIG设备
// 2. 查询结果返回的设备集合
// 3. 需要按顺序处理的设备组
type AllocatableDeviceList []*AllocatableDevice

// AllocatableDevices 是从设备规范名称到设备的映射
// 这是管理所有可分配设备的核心数据结构
// 使用map的原因：
// 1. O(1)查找：通过名称快速查找设备
// 2. 唯一性：自动确保每个设备名称只出现一次
// 3. 动态管理：方便添加和删除设备
type AllocatableDevices map[string]*AllocatableDevice

// getDevicesByGPUPCIBusID 返回与指定PCIe总线ID关联的所有设备
// 这包括：
// 1. PCIe总线ID匹配的GPU设备本身
// 2. 该GPU上的所有MIG设备（通过parent.pcieBusID匹配）
// 3. PCIe总线ID匹配的VFIO设备
//
// 使用场景：
// 1. 当分配一个设备时，需要移除同一物理GPU的其他类型设备（互斥）
// 2. 查找特定物理位置的所有可用设备
// 3. 拓扑感知调度：将相关设备分配给同一Pod
func (d AllocatableDevices) getDevicesByGPUPCIBusID(pcieBusID string) AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		switch device.Type() {
		case GpuDeviceType:
			// 检查GPU的PCIe总线ID是否匹配
			if device.Gpu.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case MigDeviceType:
			// MIG设备的PCIe总线ID继承自父GPU
			// 需要检查父GPU的PCIe总线ID
			if device.Mig.parent.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case VfioDeviceType:
			// 检查VFIO设备的PCIe总线ID是否匹配
			if device.Vfio.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		}
	}
	return devices
}

// GetGPUByPCIeBusID 通过PCIe总线ID查找GPU设备
// 只返回GpuDeviceType类型的设备，忽略MIG和VFIO设备
//
// 使用场景：
// 1. 枚举VFIO设备时，需要找到对应的物理GPU
// 2. 验证PCIe总线ID对应的GPU是否存在
// 3. 检查GPU的配置状态（如是否启用了MIG或VFIO）
func (d AllocatableDevices) GetGPUByPCIeBusID(pcieBusID string) *AllocatableDevice {
	for _, device := range d {
		// 只处理GPU类型的设备
		if device.Type() != GpuDeviceType {
			continue
		}
		// 检查PCIe总线ID是否匹配
		if device.Gpu.pcieBusID == pcieBusID {
			return device
		}
	}
	// 未找到匹配的GPU返回nil
	// 调用者需要检查nil以处理设备不存在的情况
	return nil
}

// GetGPUs 返回所有GPU设备的列表
// 只包含完整的物理GPU，不包括MIG或VFIO设备
//
// 使用场景：
// 1. 统计节点上的物理GPU数量
// 2. 遍历所有GPU以收集状态信息
// 3. 生成GPU设备清单用于上报
func (d AllocatableDevices) GetGPUs() AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		if device.Type() == GpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// GetMigDevices 返回所有MIG设备的列表
// 只包含MIG实例，不包括物理GPU或VFIO设备
//
// 使用场景：
// 1. 统计节点上的MIG设备数量
// 2. 遍历所有MIG设备以收集状态信息
// 3. 生成MIG设备清单用于上报
func (d AllocatableDevices) GetMigDevices() AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		if device.Type() == MigDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// GetVfioDevices 返回所有VFIO设备的列表
// 只包含VFIO设备，不包括物理GPU或MIG设备
//
// 使用场景：
// 1. 统计节点上可用于虚拟化的GPU数量
// 2. 遍历所有VFIO设备以收集状态信息
// 3. 生成VFIO设备清单用于上报
func (d AllocatableDevices) GetVfioDevices() AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// GpuUUIDs 返回所有GPU设备的UUID列表
// 返回的列表已排序，确保一致性和可比性
//
// 使用场景：
// 1. 生成设备指纹以检测配置变化
// 2. 与之前的设备列表比较以发现添加或移除的设备
// 3. 实现UUIDProvider接口
func (d AllocatableDevices) GpuUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == GpuDeviceType {
			uuids = append(uuids, device.Gpu.UUID)
		}
	}
	// 排序确保返回的列表顺序一致
	// 这对于比较两个UUID列表是否相同很重要
	slices.Sort(uuids)
	return uuids
}

// MigDeviceUUIDs 返回所有MIG设备的UUID列表
// 返回的列表已排序，确保一致性和可比性
//
// 使用场景：
// 1. 生成MIG设备指纹以检测配置变化
// 2. 与之前的MIG设备列表比较
// 3. 实现UUIDProvider接口
func (d AllocatableDevices) MigDeviceUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == MigDeviceType {
			uuids = append(uuids, device.Mig.UUID)
		}
	}
	// 排序确保返回的列表顺序一致
	slices.Sort(uuids)
	return uuids
}

// VfioDeviceUUIDs 返回所有VFIO设备的UUID列表
// 返回的列表已排序，确保一致性和可比性
//
// 使用场景：
// 1. 生成VFIO设备指纹以检测配置变化
// 2. 与之前的VFIO设备列表比较
// 3. 跟踪虚拟化场景中的设备分配
func (d AllocatableDevices) VfioDeviceUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			uuids = append(uuids, device.Vfio.UUID)
		}
	}
	// 排序确保返回的列表顺序一致
	slices.Sort(uuids)
	return uuids
}

// UUIDs 返回所有设备的UUID列表
// 包括GPU、MIG和VFIO设备的UUID
// 返回的列表已排序，确保一致性
//
// 使用场景：
// 1. 生成完整的设备指纹
// 2. 实现UUIDProvider接口
// 3. 检测节点上所有可分配设备的变化
func (d AllocatableDevices) UUIDs() []string {
	// 先获取GPU的UUID列表，然后追加MIG和VFIO的UUID
	uuids := append(d.GpuUUIDs(), d.MigDeviceUUIDs()...)
	uuids = append(uuids, d.VfioDeviceUUIDs()...)
	// 最后再排序一次，确保所有UUID混合后仍然有序
	slices.Sort(uuids)
	return uuids
}

// RemoveSiblingDevices 移除与指定设备在同一物理GPU上的其他类型设备
// 这实现了设备类型的互斥性：
// 1. 如果分配了完整GPU，不能再分配该GPU的MIG或VFIO设备
// 2. 如果分配了VFIO设备，不能再分配该GPU的完整GPU或MIG设备
// 3. MIG设备之间可以共存（暂不支持动态MIG，所以TODO）
//
// 为什么需要这个互斥性：
// 1. 硬件隔离：完整GPU和VFIO模式不能同时使用
// 2. 资源冲突：避免多个分配争用同一物理资源
// 3. 配置一致性：确保设备使用模式的一致性
func (d AllocatableDevices) RemoveSiblingDevices(device *AllocatableDevice) {
	// 首先确定要查找兄弟设备的PCIe总线ID
	var pciBusID string
	switch device.Type() {
	case GpuDeviceType:
		// 对于GPU设备，使用其自己的PCIe总线ID
		pciBusID = device.Gpu.pcieBusID
	case VfioDeviceType:
		// 对于VFIO设备，使用其自己的PCIe总线ID
		pciBusID = device.Vfio.pcieBusID
	case MigDeviceType:
		// TODO: Implement once dynamic MIG is supported.
		// MIG设备暂不支持移除兄弟设备
		// 原因：动态MIG功能尚未实现
		// 当前MIG配置是静态的，在GPU枚举时已确定
		return
	}

	// 获取同一PCIe总线ID的所有设备（兄弟设备）
	siblings := d.getDevicesByGPUPCIBusID(pciBusID)
	for _, sibling := range siblings {
		// 跳过与当前设备相同类型的设备
		// 例如：分配一个GPU不影响其他GPU
		if sibling.Type() == device.Type() {
			continue
		}
		// 移除不同类型的兄弟设备
		switch sibling.Type() {
		case GpuDeviceType:
			// 从map中删除GPU设备
			delete(d, sibling.Gpu.CanonicalName())
		case VfioDeviceType:
			// 从map中删除VFIO设备
			delete(d, sibling.Vfio.CanonicalName())
		case MigDeviceType:
			// TODO: Implement once dynamic MIG is supported.
			// 暂不移除MIG设备
			// 未来支持动态MIG时需要实现
			continue
		}
	}
}

// IsHealthy 返回设备是否健康
// 健康检查基于设备的Health字段
//
// 为什么只检查GPU和MIG：
// 1. VFIO设备没有Health字段，因为它们是通过PCI层面管理的
// 2. GPU和MIG设备通过NVML进行健康检查，可以检测XID错误
// 3. 不健康的设备不应该被分配给Pod
func (d *AllocatableDevice) IsHealthy() bool {
	switch d.Type() {
	case GpuDeviceType:
		// 检查GPU的健康状态
		return d.Gpu.Health == Healthy
	case MigDeviceType:
		// 检查MIG设备的健康状态
		// MIG设备的健康状态通常继承自父GPU
		return d.Mig.Health == Healthy
	}
	// VFIO设备或未知类型到达这里会panic
	// 设计上VFIO设备不应该调用IsHealthy
	// 如果调用了，说明代码逻辑有问题
	panic("unexpected type for AllocatableDevice")
}
