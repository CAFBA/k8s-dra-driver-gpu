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

/**
 *  硬件设备 (GPU)
        ↓ NVML 驱动接口
	NVIDIA 驱动 (内核态)
		↓ NVML API (用户态)
	GPU 枚举函数 (enumerateGpus)
		↓ 存储到结构体
	GpuInfo {UUID, minor, memoryBytes, productName, ...}
		↓ GetDevice() 方法
	resourceapi.Device 对象
		↓ DRA 框架
	ResourceSlice (发布到 Kubernetes API Server)
		↓ 调度器
	Pod 调度决策
*/

package main

import (
	"fmt"

	// semver包用于处理语义化版本号，确保版本号格式正确
	// 驱动版本和CUDA版本都需要遵循语义化版本规范
	"github.com/Masterminds/semver"

	// nvdev包提供NVIDIA设备库的抽象接口
	// 用于处理MIG配置文件等GPU设备相关操作
	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"

	// nvml包是NVIDIA管理库的Go绑定
	// 提供与NVIDIA GPU交互的底层API，用于查询GPU信息、创建MIG实例等
	"github.com/NVIDIA/go-nvml/pkg/nvml"

	// resourceapi是Kubernetes资源API的v1版本
	// 定义DRA框架中Device、DeviceAttribute等核心类型
	resourceapi "k8s.io/api/resource/v1"

	// resource包提供Kubernetes资源量的表示和操作
	// 用于表示GPU内存等资源容量，支持二进制(Gi)和十进制(G)单位
	"k8s.io/apimachinery/pkg/api/resource"

	// deviceattribute包提供设备属性的标准定义
	// 包括PCIe总线ID等标准属性的名称和格式
	"k8s.io/dynamic-resource-allocation/deviceattribute"

	// ptr包提供创建指针的便捷函数
	// 用于将值类型转换为指针类型，避免冗长的临时变量声明
	"k8s.io/utils/ptr"
)

// Defined similarly as https://pkg.go.dev/k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1#Healthy.
// HealthStatus 定义设备的健康状态，用于向DRA框架报告设备是否可用
type HealthStatus string

const (
	// Healthy 表示设备处于健康状态，可以正常使用
	// 设备没有检测到任何关键错误
	Healthy HealthStatus = "Healthy"

	// With NVMLDeviceHealthCheck, Unhealthy means that there are critcal xid errors on the device.
	// Unhealthy 表示设备不健康，通过NVML健康检查检测到关键的XID错误
	// XID是NVIDIA GPU的错误代码，关键XID表示硬件故障或严重问题
	Unhealthy HealthStatus = "Unhealthy"
)

// GpuInfo 存储单个物理GPU的完整信息，包含从NVML API查询到的所有重要GPU属性
// 用于向Kubernetes DRA框架报告可用的GPU资源
type GpuInfo struct {
	// UUID 是GPU的全局唯一标识符，由NVIDIA硬件提供
	UUID string `json:"uuid"`

	// minor 是GPU的设备minor号，对应/dev/nvidia{minor}
	// 用于构建设备文件路径和CDI规范
	minor int

	// migEnabled 标识GPU是否启用MIG（Multi-Instance GPU）模式
	migEnabled bool

	// vfioEnabled 标识GPU是否启用VFIO（Virtual Function I/O）
	vfioEnabled bool

	// memoryBytes 是GPU的总显存大小
	memoryBytes uint64

	// productName 是GPU的产品名称，用于设备选择器和用户识别
	productName string

	// brand 是GPU的品牌类别，如"Tesla"、"GeForce"、"Quadro"
	brand string

	// architecture 是GPU的架构代号，如"Ampere"、"Hopper"
	architecture string

	// cudaComputeCapability 是CUDA计算能力版本，如"8.0"、"9.0"
	// 决定GPU支持哪些CUDA功能和指令集，应用可能需要特定版本的计算能力
	cudaComputeCapability string

	// driverVersion 是NVIDIA驱动的版本
	driverVersion string

	// cudaDriverVersion 是CUDA驱动API的版本
	cudaDriverVersion string

	// pcieBusID 是GPU的PCIe总线地址，格式如"0000:00:1e.0"
	// 用于唯一标识物理设备位置和拓扑关系
	pcieBusID string

	// pcieRootAttr 是PCIe Root Complex 的设备属性
	// 用于表示GPU所在的PCIe拓扑结构，帮助调度器做NUMA感知决策
	pcieRootAttr *deviceattribute.DeviceAttribute

	// migProfiles 是该GPU支持的所有MIG配置文件列表
	// 每个配置文件定义一种GPU分割方式（如1g.5gb、3g.20gb）
	migProfiles []*MigProfileInfo

	// addressingMode 是GPU的寻址模式，如"physical"或"virtual"
	addressingMode *string

	// Health 是GPU的当前健康状态
	Health HealthStatus
}

// MigDeviceInfo 存储MIG（Multi-Instance GPU）设备的信息
// MIG设备是从物理GPU分割出来的独立GPU实例，每个MIG设备有自己的UUID和资源配额，由GPU实例(GI)和计算实例(CI)组成
type MigDeviceInfo struct {
	// UUID 是MIG设备的全局唯一标识符，由NVIDIA驱动生成，在MIG实例创建时分配
	UUID string `json:"uuid"`

	// profile 是MIG配置文件的字符串表示，如"1g.5gb"、"3g.20gb"
	// 格式为"{计算切片数}g.{内存大小}gb"
	profile string

	// parent 是此MIG设备所属的物理GPU
	parent *GpuInfo

	// placement 是MIG设备在GPU上的物理放置位置，定义使用哪些GPU内存切片
	placement *MigDevicePlacement

	// giProfileInfo 是GPU实例的配置文件信息，包含多处理器数量、内存大小等资源限制
	giProfileInfo *nvml.GpuInstanceProfileInfo

	// giInfo 是GPU实例的运行时信息，包含实例ID、配置文件ID等
	giInfo *nvml.GpuInstanceInfo

	// ciProfileInfo 是计算实例的配置文件信息
	ciProfileInfo *nvml.ComputeInstanceProfileInfo

	// ciInfo 是计算实例的运行时信息
	ciInfo *nvml.ComputeInstanceInfo

	// pcieBusID 继承自父GPU的PCIe总线地址，MIG设备共享父GPU的物理位置
	pcieBusID string

	// pcieRootAttr 继承自父GPU的PCIe根属性，用于NUMA感知调度
	pcieRootAttr *deviceattribute.DeviceAttribute

	// Health 是MIG设备的健康状态，通常继承父GPU的健康状态
	Health HealthStatus
}

// VfioDeviceInfo 存储VFIO（Virtual Function I/O）设备的信息
// VFIO设备用于虚拟化场景，通过VFIO框架直接分配给虚拟机
// 这允许虚拟机直接访问GPU硬件，获得接近原生的性能
type VfioDeviceInfo struct {
	// UUID 是VFIO设备的唯一标识符
	// 格式和生成方式可能与物理GPU不同
	UUID string `json:"uuid"`

	// deviceID 是PCI设备ID，标识设备型号
	// 格式如"0x20b0"，用于设备识别
	deviceID string

	// vendorID 是PCI供应商ID，对于NVIDIA通常是"0x10de"
	// 用于识别设备制造商
	vendorID string

	// index 是VFIO设备的索引号
	// 用于构建设备的规范名称
	index int

	// parent 是此VFIO设备对应的物理GPU（如果有）
	// 某些VFIO设备可能与物理GPU关联
	parent *GpuInfo

	// productName 是VFIO设备的产品名称
	productName string

	// pcieBusID 是VFIO设备的PCIe总线地址
	pcieBusID string

	// pcieRootAttr 是PCIe根复合体的属性
	// 用于NUMA感知调度
	pcieRootAttr *deviceattribute.DeviceAttribute

	// numaNode 是设备所属的NUMA节点
	// NUMA（Non-Uniform Memory Access）影响内存访问性能
	// 将Pod调度到同一NUMA节点可以提高性能
	numaNode int

	// iommuGroup 是设备的IOMMU组编号
	// IOMMU组定义了可以一起分配的设备集合
	// 这是VFIO直通的基本单位
	iommuGroup int

	// addressableMemoryBytes 是可寻址内存大小（字节）
	// 表示虚拟机可以访问的内存范围
	addressableMemoryBytes uint64
}

// MigProfileInfo 封装MIG配置文件及其可用放置位置
// 一个配置文件可能有多个可用的放置位置，有不同的内存切片组合
type MigProfileInfo struct {
	// profile 是MIG配置文件对象，包含配置文件的详细规格信息
	profile nvdev.MigProfile

	// placements 是该配置文件的所有可用放置位置，每个放置位置定义使用哪些GPU内存切片
	placements []*MigDevicePlacement
}

// MigDevicePlacement 封装MIG设备的物理放置信息
// 定义MIG设备在GPU上使用哪些内存切片
type MigDevicePlacement struct {
	// 嵌入NVML的GpuInstancePlacement结构，包含Start和Size
	// 例如：Start=0, Size=1表示使用切片0
	//       Start=4, Size=2表示使用切片4和5
	nvml.GpuInstancePlacement
}

// String 返回MIG配置文件的字符串表示
// 实现fmt.Stringer接口，方便日志输出和调试
func (p MigProfileInfo) String() string {
	return p.profile.String()
}

// CanonicalName 返回GPU设备的规范名称
// 格式为"gpu-{minor}"，如"gpu-0"、"gpu-1"
// 这个名称用于：
// 1. 在DRA资源列表中唯一标识设备
// 2. 生成CDI规范时的设备名称
// 3. 与ResourceClaim的设备选择器匹配
func (d *GpuInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%d", d.minor)
}

// CanonicalName 返回MIG设备的规范名称
// 格式为"gpu-{父GPU minor}-mig-{profile ID}-{起始切片}-{切片数量}"
// 例如："gpu-0-mig-9-0-1"表示GPU 0上的MIG设备，配置文件ID 9，使用切片0
// 这个命名方案确保：
// 1. 唯一性：不同MIG设备有不同的名称
// 2. 可读性：从名称可以推断设备的配置和位置
// 3. 稳定性：只要放置位置不变，名称就不变
func (d *MigDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%d-mig-%d-%d-%d", d.parent.minor, d.giInfo.ProfileId, d.placement.Start, d.placement.Size)
}

// CanonicalName 返回VFIO设备的规范名称
// 格式为"gpu-vfio-{index}"，如"gpu-vfio-0"、"gpu-vfio-1"
// 使用vfio前缀区分VFIO设备和普通GPU设备
func (d *VfioDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-vfio-%d", d.index)
}

// GetDevice 将GpuInfo转换为Kubernetes资源API的Device对象
// 这是DRA框架与设备信息的桥梁，用于：
// 1. 向API服务器报告可用设备及其属性
// 2. 供调度器进行资源匹配和选择
// 3. 生成ResourceSlice对象
func (d *GpuInfo) GetDevice() resourceapi.Device {
	// TODO: Consume GetPCIBusIDAttribute from https://github.com/kubernetes/kubernetes/blob/4c5746c0bc529439f78af458f8131b5def4dbe5d/staging/src/k8s.io/dynamic-resource-allocation/deviceattribute/attribute.go#L39
	// 这个 TODO 表示我们应该直接从 Kubernetes 上游库导入 GetPCIBusIDAttribute 函数，
	// 而不是手动拼接字符串。
	// 这样做可以确保属性名称与 Kubernetes 标准（特别是拓扑感知调度部分）完全一致，
	// 避免因硬编码错误导致的兼容性问题。

	// 构建PCIe总线ID属性名称，确保与其他DRA驱动兼容
	// 调度器通过 PCIe 总线 ID 查询系统拓扑时使用标准的设备属性
	pciBusIDAttrName := resourceapi.QualifiedName(deviceattribute.StandardDeviceAttributePrefix + "pciBusID")

	// 创建Device对象，包含设备的所有属性和容量信息
	device := resourceapi.Device{
		// 设备的规范名称，用于唯一标识
		Name: d.CanonicalName(),

		// Attributes 存储设备的描述性属性
		// 这些属性用于设备选择器的匹配，但不参与资源计量
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			// type属性标识设备类型为"gpu"
			// 这是最基本的分类，用于区分GPU、MIG、VFIO等不同类型
			"type": {
				StringValue: ptr.To(GpuDeviceType),
			},
			"uuid": {
				StringValue: &d.UUID,
			},
			"productName": {
				StringValue: &d.productName,
			},
			"brand": {
				StringValue: &d.brand,
			},
			"architecture": {
				StringValue: &d.architecture,
			},
			// cudaComputeCapability使用VersionValue存储
			// 因为它遵循语义化版本规范，支持版本比较
			// 先解析为语义化版本对象验证版本号的有效性，再转回字符串
			"cudaComputeCapability": {
				VersionValue: ptr.To(semver.MustParse(d.cudaComputeCapability).String()),
			},
			// driverVersion使用VersionValue存储
			"driverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.driverVersion).String()),
			},
			// cudaDriverVersion使用VersionValue存储
			"cudaDriverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.cudaDriverVersion).String()),
			},
			// pcieBusID使用标准的限定名称
			// 格式如"0000:00:1e.0"，用于物理设备定位
			pciBusIDAttrName: {
				StringValue: &d.pcieBusID,
			},
		},

		// Capacity 存储设备的可量化容量，这些值参与资源分配计算，必须是可度量的
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
			},
		},
	}

	// 如果有PCIe根属性，添加到设备属性中
	// 这用于NUMA感知调度，帮助调度器将Pod放置在GPU附近的节点上
	if d.pcieRootAttr != nil {
		device.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	// 如果有寻址模式信息，添加到设备属性中
	// 某些应用可能需要特定的寻址模式
	if d.addressingMode != nil {
		device.Attributes["addressingMode"] = resourceapi.DeviceAttribute{
			StringValue: d.addressingMode,
		}
	}

	return device
}

// GetDevice 将MigDeviceInfo转换为Kubernetes资源API的Device对象
// MIG设备有自己独特的属性集，包括配置文件信息和多种容量指标
func (d *MigDeviceInfo) GetDevice() resourceapi.Device {
	// TODO: Consume GetPCIBusIDAttribute from https://github.com/kubernetes/kubernetes/blob/4c5746c0bc529439f78af458f8131b5def4dbe5d/staging/src/k8s.io/dynamic-resource-allocation/deviceattribute/attribute.go#L39
	// 同样的，对于 MIG 设备，我们也应该使用标准的属性获取方法。
	// 这有助于维护代码的一致性和未来的可升级性。

	// 构建PCIe总线ID属性名称，与GPU设备使用相同的标准
	pciBusIDAttrName := resourceapi.QualifiedName(deviceattribute.StandardDeviceAttributePrefix + "pciBusID")

	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			// type属性标识为"mig"类型
			// 区分MIG设备和完整GPU
			"type": {
				StringValue: ptr.To(MigDeviceType),
			},
			// MIG设备的UUID
			"uuid": {
				StringValue: &d.UUID,
			},
			// parentUUID标识父GPU
			// 允许用户选择特定GPU上的MIG设备
			// 对于需要同一GPU上多个MIG设备的场景很重要
			"parentUUID": {
				StringValue: &d.parent.UUID,
			},
			// profile属性标识MIG配置文件
			// 如"1g.5gb"、"3g.20gb"
			// 用户可以按配置文件选择MIG设备
			"profile": {
				StringValue: &d.profile,
			},
			// 以下属性继承自父GPU
			// MIG设备共享父GPU的硬件特性
			"productName": {
				StringValue: &d.parent.productName,
			},
			"brand": {
				StringValue: &d.parent.brand,
			},
			"architecture": {
				StringValue: &d.parent.architecture,
			},
			"cudaComputeCapability": {
				VersionValue: ptr.To(semver.MustParse(d.parent.cudaComputeCapability).String()),
			},
			"driverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.parent.driverVersion).String()),
			},
			"cudaDriverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.parent.cudaDriverVersion).String()),
			},
			// MIG设备共享父GPU的PCIe总线地址
			pciBusIDAttrName: {
				StringValue: &d.pcieBusID,
			},
		},

		// Capacity 存储MIG设备的各种硬件资源容量
		// MIG设备有比完整GPU更细粒度的容量指标
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			// multiprocessors：流多处理器（SM）数量
			// 决定了并行计算能力
			"multiprocessors": {
				Value: *resource.NewQuantity(int64(d.giProfileInfo.MultiprocessorCount), resource.BinarySI),
			},
			// copyEngines：复制引擎数量
			// 用于内存复制操作
			"copyEngines": {Value: *resource.NewQuantity(int64(d.giProfileInfo.CopyEngineCount), resource.BinarySI)},
			// decoders：视频解码器数量
			// 用于视频解码工作负载
			"decoders": {Value: *resource.NewQuantity(int64(d.giProfileInfo.DecoderCount), resource.BinarySI)},
			// encoders：视频编码器数量
			// 用于视频编码工作负载
			"encoders": {Value: *resource.NewQuantity(int64(d.giProfileInfo.EncoderCount), resource.BinarySI)},
			// jpegEngines：JPEG引擎数量
			// 用于JPEG编解码
			"jpegEngines": {Value: *resource.NewQuantity(int64(d.giProfileInfo.JpegCount), resource.BinarySI)},
			// ofaEngines：光流加速器引擎数量
			// 用于光流计算（计算机视觉）
			"ofaEngines": {Value: *resource.NewQuantity(int64(d.giProfileInfo.OfaCount), resource.BinarySI)},
			// memory：MIG设备的显存大小
			// 从MB转换为字节（*1024*1024）
			"memory": {Value: *resource.NewQuantity(int64(d.giProfileInfo.MemorySizeMB*1024*1024), resource.BinarySI)},
		},
	}

	// 为每个内存切片添加容量指标
	// MIG设备占用特定的内存切片，这影响资源分配和冲突检测
	// 例如：如果MIG设备使用切片2-3，则添加memorySlice2和memorySlice3，各值为1
	// 调度器可以使用这些信息确保不同MIG设备的切片不冲突
	for i := d.placement.Start; i < d.placement.Start+d.placement.Size; i++ {
		capacity := resourceapi.QualifiedName(fmt.Sprintf("memorySlice%d", i))
		device.Capacity[capacity] = resourceapi.DeviceCapacity{
			// 值为1表示占用了这个切片
			// 这是一个布尔型容量，表示"是否占用"而不是"占用多少"
			Value: *resource.NewQuantity(1, resource.BinarySI),
		}
	}

	// 添加PCIe根属性
	if d.pcieRootAttr != nil {
		device.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	// 添加寻址模式，继承自父GPU
	if d.parent.addressingMode != nil {
		device.Attributes["addressingMode"] = resourceapi.DeviceAttribute{
			StringValue: d.parent.addressingMode,
		}
	}

	return device
}

// GetDevice 将VfioDeviceInfo转换为Kubernetes资源API的Device对象
// VFIO设备主要用于虚拟化场景，属性集与GPU和MIG设备不同
func (d *VfioDeviceInfo) GetDevice() resourceapi.Device {
	// TODO: Consume GetPCIBusIDAttribute from https://github.com/kubernetes/kubernetes/blob/4c5746c0bc529439f78af458f8131b5def4dbe5d/staging/src/k8s.io/dynamic-resource-allocation/deviceattribute/attribute.go#L39
	// VFIO 设备同样需要遵循标准的属性命名规范。

	// 构建PCIe总线ID属性名称
	pciBusIDAttrName := resourceapi.QualifiedName(deviceattribute.StandardDeviceAttributePrefix + "pciBusID")

	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			// type属性标识为"vfio"类型
			// 区分VFIO设备和其他类型
			"type": {
				StringValue: ptr.To(VfioDeviceType),
			},
			// VFIO设备的UUID
			"uuid": {
				StringValue: &d.UUID,
			},
			// deviceID：PCI设备ID
			// 标识设备型号，用于驱动匹配
			"deviceID": {
				StringValue: &d.deviceID,
			},
			// vendorID：PCI供应商ID
			// NVIDIA的供应商ID是0x10de
			"vendorID": {
				StringValue: &d.vendorID,
			},
			// numa：NUMA节点编号
			// 使用IntValue存储整数
			// NUMA感知调度的关键信息
			"numa": {
				IntValue: ptr.To(int64(d.numaNode)),
			},
			// PCIe总线地址
			pciBusIDAttrName: {
				StringValue: &d.pcieBusID,
			},
			// 产品名称
			"productName": {
				StringValue: &d.productName,
			},
		},

		// Capacity 存储VFIO设备的容量
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			// addressableMemory：可寻址内存大小
			// 表示虚拟机可以访问的GPU内存范围
			// 这对于虚拟化场景中的内存管理很重要
			"addressableMemory": {
				Value: *resource.NewQuantity(int64(d.addressableMemoryBytes), resource.BinarySI),
			},
		},
	}

	// 添加PCIe根属性（如果有）
	// 用于NUMA感知调度，帮助虚拟机调度器做出更好的放置决策
	if d.pcieRootAttr != nil {
		device.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	return device
}
