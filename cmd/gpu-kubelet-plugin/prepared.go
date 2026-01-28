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

import (
	"slices"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// PreparedDeviceList 是已准备设备的列表类型
// 用于存储多个 PreparedDevice，提供类型安全和便捷的过滤方法
type PreparedDeviceList []PreparedDevice

// PreparedDevices 是设备组的列表类型
// 每个设备组包含使用相同配置的设备集合
// 这种分组机制允许对共享相同配置（如 MPS、时间片设置）的设备进行批量管理
type PreparedDevices []*PreparedDeviceGroup

// PreparedDevice 表示一个已准备好的设备
// 使用联合类型（union type）模式，一次只有一个字段非 nil
// 这种设计允许在单一类型中表示三种不同的设备类型
type PreparedDevice struct {
	Gpu  *PreparedGpu        `json:"gpu"`  // 完整 GPU 设备
	Mig  *PreparedMigDevice  `json:"mig"`  // MIG（Multi-Instance GPU）设备
	Vfio *PreparedVfioDevice `json:"vfio"` // VFIO 直通设备
}

// PreparedGpu 表示已准备好的完整 GPU 设备
// 包含设备硬件信息和 kubelet 需要的设备元数据
type PreparedGpu struct {
	Info   *GpuInfo              `json:"info"`   // GPU 硬件信息（UUID、型号、内存等）
	Device *kubeletplugin.Device `json:"device"` // kubelet 设备对象，包含 CDI 设备 ID 和请求映射
}

// PreparedMigDevice 表示已准备好的 MIG 设备
// MIG 允许将一个物理 GPU 分区为多个独立的 GPU 实例
type PreparedMigDevice struct {
	Info   *MigDeviceInfo        `json:"info"`   // MIG 设备信息（包括父 GPU、GI/CI ID 等）
	Device *kubeletplugin.Device `json:"device"` // kubelet 设备对象
}

// PreparedVfioDevice 表示已准备好的 VFIO 直通设备
// VFIO 允许将设备直接分配给虚拟机或容器，提供接近裸机的性能
type PreparedVfioDevice struct {
	Info   *VfioDeviceInfo       `json:"info"`   // VFIO 设备信息（PCI 地址、IOMMU 组等）
	Device *kubeletplugin.Device `json:"device"` // kubelet 设备对象
}

// PreparedDeviceGroup 表示一组使用相同配置的已准备设备
// 分组的目的：
// 1. 共享配置：同组设备使用相同的 MPS 守护进程或时间片设置
// 2. 原子性操作：作为一个单元进行准备和清理
// 3. 资源隔离：不同组可以有不同的共享策略
type PreparedDeviceGroup struct {
	Devices     PreparedDeviceList `json:"devices"`     // 组内的所有设备
	ConfigState DeviceConfigState  `json:"configState"` // 共享的配置状态（如 MPS 守护进程 ID）
}

// Type 返回设备的类型
// 通过检查哪个字段非 nil 来确定实际类型
// 这是联合类型模式的标准实现方式
func (d PreparedDevice) Type() string {
	if d.Gpu != nil {
		return GpuDeviceType
	}
	if d.Mig != nil {
		return MigDeviceType
	}
	if d.Vfio != nil {
		return VfioDeviceType
	}
	return UnknownDeviceType
}

// CanonicalName 返回设备的规范名称
// 规范名称用于：
// 1. CDI 设备标识符
// 2. ResourceSlice 中的设备名称
// 3. 日志和调试输出
// 不同设备类型有不同的命名约定（GPU: gpu-<uuid>, MIG: mig-<uuid>, VFIO: vfio-<pci-addr>）
func (d *PreparedDevice) CanonicalName() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.Info.CanonicalName()
	case MigDeviceType:
		return d.Mig.Info.CanonicalName()
	case VfioDeviceType:
		return d.Vfio.Info.CanonicalName()
	}
	// 如果到达这里说明 Type() 返回了 UnknownDeviceType，这是编程错误
	panic("unexpected type for AllocatableDevice")
}

// Gpus 过滤出所有完整 GPU 设备
// 使用场景：
// 1. 应用时间片配置（仅适用于完整 GPU）
// 2. 启动 MPS 守护进程（需要完整 GPU）
// 3. 统计和监控
func (l PreparedDeviceList) Gpus() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == GpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// MigDevices 过滤出所有 MIG 设备
// MIG 设备有特殊的处理需求：
// 1. 需要额外的 /dev/nvidia-caps 设备节点
// 2. 不支持某些 GPU 功能（如 MPS）
// 3. 有独立的资源限制和隔离
func (l PreparedDeviceList) MigDevices() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == MigDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// VfioDevices 过滤出所有 VFIO 设备
// VFIO 设备处理流程完全不同：
// 1. 需要将设备从 nvidia 驱动解绑到 vfio-pci 驱动
// 2. 需要配置 IOMMU
// 3. 清理时需要重新绑定回 nvidia 驱动
func (l PreparedDeviceList) VfioDevices() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == VfioDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// GetDevices 收集所有设备组中的 kubelet 设备对象
// 这个方法将嵌套的设备组结构展平为 kubelet 需要的简单列表
// kubelet 使用这些设备对象来：
// 1. 更新容器运行时配置
// 2. 注入 CDI 设备到容器
// 3. 跟踪资源分配
func (d PreparedDevices) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, group := range d {
		devices = append(devices, group.GetDevices()...)
	}
	return devices
}

// GetDevices 从单个设备组中提取 kubelet 设备对象
// 根据设备类型从相应的字段中提取 Device 指针
// 解引用是必要的，因为 kubelet 期望值类型而不是指针
func (g *PreparedDeviceGroup) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, device := range g.Devices {
		switch device.Type() {
		case GpuDeviceType:
			devices = append(devices, *device.Gpu.Device)
		case MigDeviceType:
			devices = append(devices, *device.Mig.Device)
		case VfioDeviceType:
			devices = append(devices, *device.Vfio.Device)
		}
	}
	return devices
}

// UUIDs 返回设备列表中所有设备的 UUID（已排序）
// 排序的目的：
// 1. 确定性输出，便于测试和调试
// 2. 可预测的日志格式
// 3. 便于比较和差异检测
func (l PreparedDeviceList) UUIDs() []string {
	uuids := append(l.GpuUUIDs(), l.MigDeviceUUIDs()...)
	uuids = append(uuids, l.VfioDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

// UUIDs 返回设备组中所有设备的 UUID（已排序）
// 设备组的 UUID 列表用于：
// 1. 生成 MPS 守护进程配置
// 2. 日志记录哪些设备被一起配置
// 3. 调试共享配置问题
func (g *PreparedDeviceGroup) UUIDs() []string {
	uuids := append(g.GpuUUIDs(), g.MigDeviceUUIDs()...)
	uuids = append(uuids, g.VfioDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

// UUIDs 返回所有设备组中所有设备的 UUID（已排序）
// 这是最顶层的 UUID 聚合，用于：
// 1. 生成 claim 级别的唯一标识
// 2. 审计和跟踪
// 3. checkpoint 数据验证
func (d PreparedDevices) UUIDs() []string {
	uuids := append(d.GpuUUIDs(), d.MigDeviceUUIDs()...)
	uuids = append(uuids, d.VfioDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

// GpuUUIDs 提取并排序设备列表中所有完整 GPU 的 UUID
// GPU UUID 是 NVIDIA 驱动分配的全局唯一标识符
// 格式：GPU-<8位十六进制>-<4位>-<4位>-<4位>-<12位>
func (l PreparedDeviceList) GpuUUIDs() []string {
	var uuids []string
	for _, device := range l.Gpus() {
		uuids = append(uuids, device.Gpu.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

// GpuUUIDs 返回设备组中所有完整 GPU 的 UUID
// 委托给设备列表的实现以保持代码 DRY（Don't Repeat Yourself）
func (g *PreparedDeviceGroup) GpuUUIDs() []string {
	return g.Devices.Gpus().UUIDs()
}

// GpuUUIDs 返回所有设备组中所有完整 GPU 的 UUID
// 用于生成 claim 级别的 GPU 列表
func (d PreparedDevices) GpuUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.GpuUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}

// MigDeviceUUIDs 提取并排序设备列表中所有 MIG 设备的 UUID
// MIG UUID 格式：MIG-<parent-gpu-uuid>/GI-<gi-id>/CI-<ci-id>
// 这种分层格式反映了 MIG 的两级分区结构（GPU Instance -> Compute Instance）
func (l PreparedDeviceList) MigDeviceUUIDs() []string {
	var uuids []string
	for _, device := range l.MigDevices() {
		uuids = append(uuids, device.Mig.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

// MigDeviceUUIDs 返回设备组中所有 MIG 设备的 UUID
func (g *PreparedDeviceGroup) MigDeviceUUIDs() []string {
	return g.Devices.MigDevices().UUIDs()
}

// MigDeviceUUIDs 返回所有设备组中所有 MIG 设备的 UUID
func (d PreparedDevices) MigDeviceUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.MigDeviceUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}

// VfioDeviceUUIDs 返回设备组中所有 VFIO 设备的 UUID
func (g *PreparedDeviceGroup) VfioDeviceUUIDs() []string {
	return g.Devices.VfioDevices().UUIDs()
}

// VfioDeviceUUIDs 提取并排序设备列表中所有 VFIO 设备的 UUID
// VFIO UUID 通常基于 PCI 地址生成，确保与物理硬件的稳定映射
func (l PreparedDeviceList) VfioDeviceUUIDs() []string {
	var uuids []string
	for _, device := range l.VfioDevices() {
		uuids = append(uuids, device.Vfio.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

// VfioDeviceUUIDs 返回所有设备组中所有 VFIO 设备的 UUID
func (d PreparedDevices) VfioDeviceUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.VfioDeviceUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}

// GetNonAdminDevices returns a map of device names that were requested
// without admin access in the prepared claim.
func (c *PreparedClaim) GetNonAdminDevices() map[string]struct{} {
	requested := make(map[string]struct{}, len(c.Status.Allocation.Devices.Results))

	if c.Status.Allocation == nil {
		return requested
	}
	for _, r := range c.Status.Allocation.Devices.Results {
		if r.Driver != DriverName {
			continue
		}
		if r.AdminAccess != nil && *r.AdminAccess {
			continue
		}
		requested[r.Device] = struct{}{}
	}
	return requested
}
