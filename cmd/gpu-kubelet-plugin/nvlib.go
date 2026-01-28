/*
 * Copyright (c) 2021-2025, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	// uuid包用于为VFIO设备生成基于地址的UUID
	"github.com/google/uuid"

	"k8s.io/klog/v2"

	// nvdev提供NVIDIA设备库的高级抽象
	// 简化与NVML的交互，提供更友好的API
	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"

	// nvpci提供NVIDIA PCI设备的枚举和信息查询
	// 用于发现VFIO设备
	"github.com/NVIDIA/go-nvlib/pkg/nvpci"

	// nvml是NVIDIA管理库的Go绑定，提供与GPU交互的底层API
	"github.com/NVIDIA/go-nvml/pkg/nvml"

	// deviceattribute提供设备属性的标准定义
	// 用于获取PCIe根复合体信息
	"k8s.io/dynamic-resource-allocation/deviceattribute"

	// featuregates提供特性门控机制
	// 用于控制实验性功能（如VFIO直通）的启用
	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/featuregates"
)

// deviceLib 封装与NVIDIA设备交互所需的所有接口和配置，这是设备枚举和管理的核心组件
type deviceLib struct {
	// nvdev.Interface 嵌入NVIDIA设备库接口
	// 提供VisitDevices、NewMigProfile等高级方法
	nvdev.Interface

	// nvmllib 是NVML库的接口
	// 用于直接调用NVML API，如Init、Shutdown等
	nvmllib nvml.Interface

	// nvpci 是NVIDIA PCI设备接口
	// 用于枚举和查询PCI层面的GPU设备信息
	nvpci nvpci.Interface

	// driverLibraryPath 是NVIDIA驱动库（libnvidia-ml.so.1）的路径
	// 需要显式指定以避免依赖系统库路径配置
	driverLibraryPath string

	// devRoot 是设备文件的根目录
	// 通常是"/dev"，但在容器环境中可能不同
	devRoot string

	// nvidiaSMIPath 是nvidia-smi工具的路径
	// 用于设置计算模式和时间片等高级配置
	nvidiaSMIPath string
}

// newDeviceLib 创建一个新的设备库实例
// 参数:
//   - driverRoot: 驱动根目录接口，用于定位驱动文件和工具
//
// 返回:
//   - *deviceLib: 设备库实例
//   - error: 创建过程中的错误
//
// 1. 这个函数负责初始化所有与NVIDIA设备交互所需的组件
// 2. 显式指定库路径而不依赖系统的动态链接器搜索路径，这样做的原因是:
//   - 确保加载正确版本的驱动库(避免版本冲突)
//   - 在容器环境中驱动库可能不在标准路径
//   - 提高可移植性和可预测性
func newDeviceLib(driverRoot root) (*deviceLib, error) {
	// 获取驱动库路径(libnvidia-ml.so.1等)
	// 为什么需要: NVML库是与GPU交互的核心，必须确保找到正确的版本
	driverLibraryPath, err := driverRoot.getDriverLibraryPath()
	if err != nil {
		return nil, fmt.Errorf("failed to locate driver libraries: %w", err)
	}

	// 获取nvidia-smi工具路径
	// 为什么需要: nvidia-smi用于设置GPU配置(如计算模式、时间片等)
	// 这些操作无法通过NVML API完成，必须使用CLI工具
	nvidiaSMIPath, err := driverRoot.getNvidiaSMIPath()
	if err != nil {
		return nil, fmt.Errorf("failed to locate nvidia-smi: %w", err)
	}

	// We construct an NVML library specifying the path to libnvidia-ml.so.1
	// explicitly so that we don't have to rely on the library path.
	// 显式构造NVML库实例，指定libnvidia-ml.so.1的路径
	// 为什么这样做: 不依赖LD_LIBRARY_PATH等环境变量，避免加载错误版本的库
	nvmllib := nvml.New(
		nvml.WithLibraryPath(driverLibraryPath),
	)

	// 创建PCI设备接口
	// 为什么需要: 用于枚举PCIe总线上的GPU设备，获取VFIO直通所需的信息
	nvpci := nvpci.New()

	// 组装deviceLib结构
	// Interface: 提供设备枚举和查询的高级接口
	// nvmllib: 底层NVML库，用于直接调用NVML API
	// driverLibraryPath: 保存以便后续需要设置LD_PRELOAD时使用
	// devRoot: 设备节点根目录(通常是/dev)
	// nvidiaSMIPath: nvidia-smi路径，用于执行配置命令
	// nvpci: PCI设备接口，用于VFIO设备发现
	d := deviceLib{
		Interface:         nvdev.New(nvmllib),
		nvmllib:           nvmllib,
		driverLibraryPath: driverLibraryPath,
		devRoot:           driverRoot.getDevRoot(),
		nvidiaSMIPath:     nvidiaSMIPath,
		nvpci:             nvpci,
	}
	return &d, nil
}

// prependPathListEnvvar prepends a specified list of strings to a specified envvar and returns its value.
// prependPathListEnvvar 将指定的字符串列表前置到环境变量值中并返回
// 参数:
//   - envvar: 环境变量名(如"LD_LIBRARY_PATH")
//   - prepend: 要前置的路径列表
//
// 返回: 更新后的环境变量值
// 1. 使用filepath.ListSeparator确保跨平台兼容(Linux用:, Windows用;)
// 2. 前置而非追加的原因: 确保优先使用指定的路径，避免加载系统中的旧版本库
// 3. 这在容器环境中特别重要，因为主机和容器可能有不同版本的驱动
func prependPathListEnvvar(envvar string, prepend ...string) string {
	// 如果没有要前置的内容，直接返回当前值
	if len(prepend) == 0 {
		return os.Getenv(envvar)
	}
	// 分割当前环境变量值(按平台特定的分隔符)
	current := filepath.SplitList(os.Getenv(envvar))
	// 将新路径前置到现有路径之前，然后用分隔符连接
	return strings.Join(append(prepend, current...), string(filepath.ListSeparator))
}

// setOrOverrideEnvvar adds or updates an envar to the list of specified envvars and returns it.
// setOrOverrideEnvvar 在环境变量列表中添加或更新指定的环境变量
// 参数:
//   - envvars: 现有的环境变量列表(格式: ["KEY=VALUE", ...])
//   - key: 要设置的环境变量名
//   - value: 要设置的值
//
// 返回: 更新后的环境变量列表
// 1. 这个函数用于准备子进程(如nvidia-smi)的环境变量
// 2. 如果key已存在则覆盖(通过跳过旧值实现)
// 3. 最终将新的key=value追加到列表末尾
// 4. 为什么需要这个函数: os/exec包接受环境变量列表，需要确保特定变量被正确设置
func setOrOverrideEnvvar(envvars []string, key, value string) []string {
	var updated []string
	// 遍历现有环境变量，过滤掉要更新的key
	for _, envvar := range envvars {
		// 按=分割，最多分割成2部分(key和value)
		pair := strings.SplitN(envvar, "=", 2)
		if pair[0] == key {
			// 跳过旧值，实现"覆盖"效果
			continue
		}
		updated = append(updated, envvar)
	}
	// 追加新的key=value
	return append(updated, fmt.Sprintf("%s=%s", key, value))
}

// Init 初始化NVML库
//
// 1. NVML必须先初始化才能使用任何GPU查询功能
// 2. 初始化会加载驱动库并建立与GPU的通信
// 3. 这个方法应该在每次需要查询GPU信息前调用
// 4. 对应的alwaysShutdown()必须在使用完成后调用以释放资源
func (l deviceLib) Init() error {
	ret := l.nvmllib.Init()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error initializing NVML: %v", ret)
	}
	return nil
}

// alwaysShutdown 关闭NVML库并释放资源
//
// 1. 名为"alwaysShutdown"而不是"Shutdown"的原因:
//   - 强调这个函数应该总是被调用(通常通过defer)
//   - 即使在错误情况下也要确保资源被释放
//
// 2. 使用Warning而不是Error记录失败:
//   - 关闭失败通常不影响程序继续运行
//   - 但仍需记录以便调试潜在的资源泄漏问题
//
// 3. 典型用法: defer l.alwaysShutdown()
func (l deviceLib) alwaysShutdown() {
	ret := l.nvmllib.Shutdown()
	if ret != nvml.SUCCESS {
		klog.Warningf("error shutting down NVML: %v", ret)
	}
}

// enumerateAllPossibleDevices 枚举所有可能的GPU设备(包括GPU、MIG和VFIO设备)
// 参数:
//   - config: 插件配置
//
// 返回:
//   - AllocatableDevices: 所有可分配的设备映射
//   - error: 枚举过程中的错误
//
// 1. 这是设备发现的顶层函数，协调不同类型设备的枚举
// 2. 为什么分开枚举GPU/MIG和VFIO设备:
//   - GPU/MIG通过NVML库发现(需要驱动支持)
//   - VFIO设备通过PCI子系统发现(绕过驱动，用于虚拟化直通)
//
// 3. PassthroughSupport特性门控的原因:
//   - VFIO直通是高级功能，不是所有环境都支持
//   - 需要特殊的内核配置和硬件支持(IOMMU)
//   - 通过特性门控可以在不支持的环境中禁用此功能
//
// 4. 返回的alldevices包含所有类型的设备，供后续分配使用
func (l deviceLib) enumerateAllPossibleDevices(config *Config) (AllocatableDevices, error) {
	alldevices := make(AllocatableDevices)

	// 首先枚举GPU和MIG设备
	// 为什么先枚举这些: 它们是基础设备类型，VFIO设备的发现依赖于GPU信息
	gms, err := l.enumerateGpusAndMigDevices(config)
	if err != nil {
		return nil, fmt.Errorf("error enumerating GPUs and MIG devices: %w", err)
	}
	// 将GPU和MIG设备添加到总设备列表
	for k, v := range gms {
		alldevices[k] = v
	}

	// 如果启用了直通支持，枚举VFIO设备
	// 为什么有特性门控: VFIO设备发现和管理复杂，且不是所有场景都需要
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		// 枚举GPU PCI设备(用于VFIO直通)
		// 传入gms的原因: 需要知道哪些GPU已被发现，以便关联VFIO设备与父GPU
		passthroughDevices, err := l.enumerateGpuPciDevices(config, gms)
		if err != nil {
			return nil, fmt.Errorf("error enumerating GPU PCI devices: %w", err)
		}
		// 将VFIO设备添加到总设备列表
		for k, v := range passthroughDevices {
			alldevices[k] = v
		}
	}
	return alldevices, nil
}

// enumerateGpusAndMigDevices 枚举所有 GPU 和 MIG 设备
// 参数:
//   - config: 插件配置（当前未使用，但保留用于未来扩展）
//
// 返回:
//   - AllocatableDevices: 发现的 GPU 和 MIG 设备映射
//   - error: 枚举过程中的错误
//
// 设计说明:
// 1. 为什么需要这个函数:
//   - 这是设备发现的核心逻辑，负责识别系统中的所有 NVIDIA GPU
//   - 处理 MIG（Multi-Instance GPU）设备的发现和层级关系
//   - 决定是否允许 VFIO 直通（基于 MIG 状态）
//
// 2. MIG 处理逻辑:
//   - 如果 GPU 未启用 MIG: 直接将整个 GPU 作为一个可分配设备
//   - 如果 GPU 启用 MIG:
//   - 发现其上的所有 MIG 实例
//   - 将每个 MIG 实例作为独立的可分配设备
//   - 父 GPU 本身不再作为可分配设备（避免资源冲突）
//
// 3. VFIO 互斥逻辑:
//   - 只有当 GPU 没有创建任何 MIG 设备时，才允许 VFIO 直通
//   - 可以在启用 MIG 模式但未创建实例的情况下使用 VFIO（虽然不常见）
func (l deviceLib) enumerateGpusAndMigDevices(config *Config) (AllocatableDevices, error) {
	// 初始化 NVML
	if err := l.Init(); err != nil {
		return nil, err
	}
	defer l.alwaysShutdown()

	devices := make(AllocatableDevices)
	// 遍历系统中的每个 GPU 设备
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		// 获取 GPU 的详细信息（型号、UUID、内存等）
		gpuInfo, err := l.getGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		// 创建父设备对象（代表物理 GPU）
		parentdev := &AllocatableDevice{
			Gpu: gpuInfo,
		}

		// 发现该 GPU 上的所有 MIG 设备
		migdevs, err := l.discoverMigDevicesByGPU(gpuInfo)
		if err != nil {
			return fmt.Errorf("error discovering MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
		}

		// 处理 VFIO 直通支持
		if featuregates.Enabled(featuregates.PassthroughSupport) {
			// If no MIG devices are found, allow VFIO devices.
			// 只有在没有发现 MIG 设备的情况下才启用 VFIO
			// 为什么: 硬件限制，通常不能同时使用 MIG 分区和将整个 PF 直通给虚拟机
			gpuInfo.vfioEnabled = len(migdevs) == 0
		}

		// 情况 1: GPU 未启用 MIG 模式
		// 这是一个标准的完整 GPU，直接添加到可分配设备列表
		if !gpuInfo.migEnabled {
			klog.Infof("Adding device %s to allocatable devices", gpuInfo.CanonicalName())
			devices[gpuInfo.CanonicalName()] = parentdev
			return nil
		}

		// 情况 2: GPU 启用了 MIG 模式
		// Likely unintentionally stranded capacity (misconfiguration).
		// 如果启用了 MIG 但没有创建实例，这是一个警告状态
		// 可能是配置错误，或者用户忘记创建 MIG 实例
		if len(migdevs) == 0 {
			klog.Warningf("Physical GPU %s has MIG mode enabled but no configured MIG devices", gpuInfo.CanonicalName())
		}

		// 将所有发现的 MIG 设备添加到可分配列表
		// 注意：在这种情况下，父 GPU 不会被添加，只有其子 MIG 设备被添加
		for _, mdev := range migdevs {
			klog.Infof("Adding MIG device %s to allocatable devices (parent: %s)", mdev.CanonicalName(), gpuInfo.CanonicalName())
			devices[mdev.CanonicalName()] = mdev
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error visiting devices: %w", err)
	}

	return devices, nil
}

// discoverMigDevicesByGPU 发现指定GPU上的所有MIG设备
// 参数:
//   - gpuInfo: 父GPU的信息
//
// 返回:
//   - AllocatableDeviceList: MIG设备列表
//   - error: 发现过程中的错误
//
// 设计说明:
// 1. 这是一个简单的适配器函数，将MIG设备信息转换为可分配设备列表
// 2. 为什么需要这个函数而不直接使用getMigDevices:
//   - 类型转换: getMigDevices返回map，这里需要list
//   - 包装: 需要将MigDeviceInfo包装成AllocatableDevice
//   - 抽象: 提供更高级的接口，隐藏底层实现细节
func (l deviceLib) discoverMigDevicesByGPU(gpuInfo *GpuInfo) (AllocatableDeviceList, error) {
	var devices AllocatableDeviceList
	// 获取GPU上的所有MIG设备(返回map[UUID]*MigDeviceInfo)
	migs, err := l.getMigDevices(gpuInfo)
	if err != nil {
		return nil, fmt.Errorf("error getting MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
	}

	// 将每个MIG设备信息包装成AllocatableDevice
	// 为什么需要包装: AllocatableDevice是统一的设备表示，可以容纳不同类型的设备
	for _, migDeviceInfo := range migs {
		mig := &AllocatableDevice{
			Mig: migDeviceInfo,
		}
		devices = append(devices, mig)
	}
	return devices, nil
}

// TODO: Need go-nvlib util for this.
// 这个 TODO 表示当前的实现（通过遍历所有设备来查找特定 PCI Bus ID 的设备）效率较低且繁琐。
// 理想情况下，上游库 `go-nvlib` 应该提供类似 `GetDeviceByPCIBusID(id)` 的直接 API。
// 当上游库添加此功能后，这里的代码应该被重构以简化逻辑。

// discoverGPUByPCIBusID 通过PCIe总线ID发现GPU及其MIG设备
// 参数:
//   - pcieBusID: PCIe总线ID(格式: "0000:01:00.0")
//
// 返回:
//   - *AllocatableDevice: 找到的GPU设备
//   - AllocatableDeviceList: GPU上的MIG设备列表
//   - error: 发现过程中的错误
//
// 设计说明:
// 1. 为什么需要这个函数:
//   - 用于通过PCIe总线ID查找特定的GPU
//   - 在处理VFIO设备时需要关联PCI设备和GPU设备
//
// 2. 为什么同时返回GPU和MIG设备:
//   - 调用者可能需要知道GPU上有哪些MIG设备
//   - 用于确定是否可以启用VFIO(只有在没有MIG时才允许)
func (l deviceLib) discoverGPUByPCIBusID(pcieBusID string) (*AllocatableDevice, AllocatableDeviceList, error) {
	// 初始化NVML以查询GPU信息
	if err := l.Init(); err != nil {
		return nil, nil, err
	}
	defer l.alwaysShutdown()

	var gpu *AllocatableDevice
	var migs AllocatableDeviceList
	// 遍历所有GPU设备查找匹配的PCIe总线ID
	// 为什么需要遍历: NVML没有直接通过PCIe ID查找GPU的API
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		// 获取当前设备的PCIe总线ID
		gpuPCIBusID, err := d.GetPCIBusID()
		if err != nil {
			return fmt.Errorf("error getting PCIe bus ID for device %d: %w", i, err)
		}
		// 如果不匹配，继续查找下一个设备
		if gpuPCIBusID != pcieBusID {
			return nil
		}
		// 找到匹配的GPU，获取其详细信息
		gpuInfo, err := l.getGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}
		// 发现GPU上的MIG设备
		migs, err = l.discoverMigDevicesByGPU(gpuInfo)
		if err != nil {
			return fmt.Errorf("error discovering MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
		}
		// If no MIG devices are found, allow VFIO devices.
		// 设置VFIO启用标志
		// 为什么基于MIG设备数量: MIG和VFIO不能共存，只有在没有MIG时才能使用VFIO
		gpuInfo.vfioEnabled = len(migs) == 0
		gpu = &AllocatableDevice{
			Gpu: gpuInfo,
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("error visiting devices: %w", err)
	}
	return gpu, migs, nil
}

// TODO: Need go-nvlib util for this.
// 同样的，这个 TODO 也表示当前的查找逻辑（遍历所有 PCI 设备进行匹配）可以优化。
// 如果 `go-nvlib` 或底层库能提供通过 Bus ID 直接获取 PCI 设备信息的 API，
// 这里的代码可以大大简化。

// discoverVfioDevice 发现指定GPU的VFIO设备
// 参数:
//   - gpuInfo: GPU信息
//
// 返回:
//   - *AllocatableDevice: VFIO设备(如果找到)
//   - error: 发现过程中的错误
//
// 设计说明:
// 1. 为什么需要这个函数:
//   - VFIO设备用于GPU直通(passthrough)到虚拟机或容器
//   - 需要关联PCI设备信息和GPU信息
//
// 2. 为什么通过PCIe总线ID匹配:
//   - PCIe总线ID是GPU在系统中的物理地址
//   - 同一个GPU在NVML和PCI子系统中有相同的总线ID
func (l deviceLib) discoverVfioDevice(gpuInfo *GpuInfo) (*AllocatableDevice, error) {
	// 获取所有GPU PCI设备
	// 为什么使用nvpci: PCI接口可以访问硬件拓扑信息，这对VFIO设备发现很重要
	gpus, err := l.nvpci.GetGPUs()
	if err != nil {
		return nil, fmt.Errorf("error getting GPU PCI devices: %w", err)
	}
	// 遍历PCI设备查找匹配的GPU
	for idx, gpu := range gpus {
		// 通过PCIe总线ID匹配GPU
		if gpu.Address != gpuInfo.pcieBusID {
			continue
		}
		// 获取VFIO设备信息
		vfioDeviceInfo, err := l.getVfioDeviceInfo(idx, gpu)
		if err != nil {
			return nil, fmt.Errorf("error getting VFIO device info: %w", err)
		}
		// 设置父GPU关联
		// 为什么需要parent: VFIO设备需要知道它对应的物理GPU，用于资源跟踪
		vfioDeviceInfo.parent = gpuInfo
		return &AllocatableDevice{
			Vfio: vfioDeviceInfo,
		}, nil
	}
	return nil, fmt.Errorf("error discovering VFIO device by PCIe bus ID: %s", gpuInfo.pcieBusID)
}

// getGpuInfo 获取GPU的详细信息
// 参数:
//   - index: GPU索引(在系统GPU列表中的位置)
//   - device: NVML设备句柄
//
// 返回:
//   - *GpuInfo: GPU信息结构
//   - error: 获取过程中的错误
//
// 设计说明:
// 1. 这个函数收集GPU的所有重要属性，包括:
//   - 识别信息: UUID, minor设备号
//   - 能力信息: 内存大小, 架构, CUDA计算能力
//   - 配置信息: MIG模式, 寻址模式
//   - 拓扑信息: PCIe总线ID, PCIe根位置
//   - MIG配置: 支持的MIG profile及其放置选项
//
// 2. 为什么收集这么多信息:
//   - DRA需要这些属性来匹配用户的设备请求
//   - Kubernetes调度器使用这些信息进行设备选择
//   - 容器运行时需要这些信息来配置设备访问
func (l deviceLib) getGpuInfo(index int, device nvdev.Device) (*GpuInfo, error) {
	// 获取minor设备号
	// 为什么需要: minor号用于构造设备节点路径(/dev/nvidia0, /dev/nvidia1等)
	minor, ret := device.GetMinorNumber()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting minor number for device %d: %v", index, ret)
	}

	// 获取GPU UUID(全局唯一标识符)
	// 为什么需要: UUID是GPU的持久标识符，即使设备号改变也不会变
	uuid, ret := device.GetUUID()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting UUID for device %d: %v", index, ret)
	}

	// 检查MIG模式是否启用
	// 为什么重要: MIG模式决定GPU是否被分区，影响如何分配设备
	migEnabled, err := device.IsMigEnabled()
	if err != nil {
		return nil, fmt.Errorf("error checking if MIG mode enabled for device %d: %w", index, err)
	}

	// 获取内存信息(总量和已用量)
	// 为什么需要: 内存大小是设备分配的重要资源维度
	memory, ret := device.GetMemoryInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting memory info for device %d: %v", index, ret)
	}

	// 获取产品名称(如"Tesla V100", "A100-SXM4-40GB")
	// 为什么需要: 用户可能根据GPU型号请求设备
	productName, ret := device.GetName()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting product name for device %d: %v", index, ret)
	}

	// 获取GPU架构(如"Ampere", "Hopper")
	// 为什么需要: 不同架构有不同的特性，用户可能有架构要求
	architecture, err := device.GetArchitectureAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting architecture for device %d: %w", index, err)
	}

	// 获取GPU品牌(如"Tesla", "GeForce")
	// 为什么需要: 品牌反映GPU的目标市场(数据中心vs消费级)
	brand, err := device.GetBrandAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting brand for device %d: %w", index, err)
	}

	// 获取CUDA计算能力(如"8.0", "9.0")
	// 为什么需要: CUDA程序可能要求最低计算能力
	cudaComputeCapability, err := device.GetCudaComputeCapabilityAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting CUDA compute capability for device %d: %w", index, err)
	}

	// 获取驱动版本
	// 为什么需要: 应用可能依赖特定驱动版本的功能
	driverVersion, ret := l.nvmllib.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting driver version: %w", err)
	}

	// 获取CUDA驱动版本
	// 为什么需要: CUDA应用需要兼容的驱动版本
	cudaDriverVersion, ret := l.nvmllib.SystemGetCudaDriverVersion()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting CUDA driver version: %w", err)
	}

	// 获取PCIe总线ID
	// 为什么需要: 用于关联VFIO设备和识别物理位置(NUMA节点)
	pcieBusID, err := device.GetPCIBusID()
	if err != nil {
		return nil, fmt.Errorf("error getting PCIe bus ID for device %d: %w", index, err)
	}

	// Get the memory-addressing mode supported by the device.
	// On coherent-memory systems, the possible modes are:
	//   - HMM  (Hardware Memory Management)
	//   - ATS  (Address Translation Service)
	//   - None (Supported by the platform but currently inactive)
	//   - ""   (Not supported by the platform)
	// 获取设备支持的内存寻址模式
	// 在一致性内存系统上，可能的模式有:
	//   - HMM  (硬件内存管理): GPU可以访问系统内存页
	//   - ATS  (地址转换服务): PCIe设备使用系统页表
	//   - None (平台支持但当前未激活)
	//   - ""   (平台不支持)
	// 为什么重要: 影响GPU内存访问性能和统一内存功能
	var addressingMode *string
	if mode, err := device.GetAddressingModeAsString(); err != nil {
		return nil, fmt.Errorf("error getting addressing mode for device %d: %w", index, err)
	} else if mode != "" {
		// 只有在模式非空时才设置(空字符串表示不支持)
		addressingMode = &mode
	}

	// 获取PCIe根复杂设备属性(用于NUMA感知调度)
	// 为什么需要: PCIe根位置决定了GPU与CPU/内存的距离，影响性能
	var pcieRootAttr *deviceattribute.DeviceAttribute
	if attr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(pcieBusID); err == nil {
		pcieRootAttr = &attr
	} else {
		// 如果无法获取也继续执行，只是没有NUMA感知能力
		klog.Warningf("error getting PCIe root for device %d, continuing without attribute: %v", index, err)
	}

	var migProfiles []*MigProfileInfo
	for i := 0; i < nvml.GPU_INSTANCE_PROFILE_COUNT; i++ {
		giProfileInfo, ret := device.GetGpuInstanceProfileInfo(i)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("error retrieving GpuInstanceProfileInfo for profile %d on GPU %v", i, uuid)
		}

		giPossiblePlacements, ret := device.GetGpuInstancePossiblePlacements(&giProfileInfo)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("error retrieving GpuInstancePossiblePlacements for profile %d on GPU %v", i, uuid)
		}

		var migDevicePlacements []*MigDevicePlacement
		for _, p := range giPossiblePlacements {
			mdp := &MigDevicePlacement{
				GpuInstancePlacement: p,
			}
			migDevicePlacements = append(migDevicePlacements, mdp)
		}

		for j := 0; j < nvml.COMPUTE_INSTANCE_PROFILE_COUNT; j++ {
			for k := 0; k < nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_COUNT; k++ {
				migProfile, err := l.NewMigProfile(i, j, k, giProfileInfo.MemorySizeMB, memory.Total)
				if err != nil {
					return nil, fmt.Errorf("error building MIG profile from GpuInstanceProfileInfo for profile %d on GPU %v", i, uuid)
				}

				if migProfile.GetInfo().G != migProfile.GetInfo().C {
					continue
				}

				profileInfo := &MigProfileInfo{
					profile:    migProfile,
					placements: migDevicePlacements,
				}

				migProfiles = append(migProfiles, profileInfo)
			}
		}
	}

	gpuInfo := &GpuInfo{
		UUID:                  uuid,
		minor:                 minor,
		migEnabled:            migEnabled,
		memoryBytes:           memory.Total,
		productName:           productName,
		brand:                 brand,
		architecture:          architecture,
		cudaComputeCapability: cudaComputeCapability,
		driverVersion:         driverVersion,
		cudaDriverVersion:     fmt.Sprintf("%v.%v", cudaDriverVersion/1000, (cudaDriverVersion%1000)/10),
		pcieBusID:             pcieBusID,
		pcieRootAttr:          pcieRootAttr,
		migProfiles:           migProfiles,
		Health:                Healthy,
		addressingMode:        addressingMode,
	}

	return gpuInfo, nil
}

// enumerateGpuPciDevices 枚举GPU PCI设备并创建VFIO设备
// 参数:
//   - config: 插件配置
//   - gms: 已枚举的GPU和MIG设备(用于关联VFIO设备与父GPU)
//
// 返回:
//   - AllocatableDevices: VFIO设备映射
//   - error: 枚举过程中的错误
//
// 设计说明:
// 1. 为什么需要传入gms参数:
//   - 需要检查GPU是否允许VFIO(通过vfioEnabled标志)
//   - 需要关联VFIO设备与对应的GPU
//
// 2. 为什么过滤vfioEnabled:
//   - 只有未启用MIG的GPU才能使用VFIO直通
//   - 这是在enumerateGpusAndMigDevices中设置的互斥条件
//
// 3. VFIO设备的用途:
//   - 允许GPU直通到虚拟机或特权容器
//   - 提供设备级别的隔离(而不是共享GPU)
func (l deviceLib) enumerateGpuPciDevices(config *Config, gms AllocatableDevices) (AllocatableDevices, error) {
	devices := make(AllocatableDevices)
	// 获取所有GPU PCI设备
	// 为什么使用PCI接口: VFIO需要PCI级别的信息(如IOMMU组、NUMA节点)
	gpuPciDevices, err := l.nvpci.GetGPUs()
	if err != nil {
		return nil, fmt.Errorf("error getting GPU PCI devices: %w", err)
	}
	// 遍历每个PCI设备
	for idx, pci := range gpuPciDevices {
		// 查找对应的父GPU
		// 为什么通过PCIe总线ID查找: 这是PCI设备和GPU设备之间的共同标识符
		parent := gms.GetGPUByPCIeBusID(pci.Address)
		if parent == nil || !parent.Gpu.vfioEnabled {
			// 跳过以下情况:
			// 1. parent == nil: 在NVML中找不到对应的GPU(可能是非NVIDIA设备)
			// 2. !vfioEnabled: GPU启用了MIG或其他原因不允许VFIO
			continue
		}
		// 创建VFIO设备信息
		vfioDeviceInfo, err := l.getVfioDeviceInfo(idx, pci)
		if err != nil {
			return nil, fmt.Errorf("error getting GPU info from PCI device: %w", err)
		}
		// 设置父GPU关联
		vfioDeviceInfo.parent = parent.Gpu
		// 添加到设备列表
		devices[vfioDeviceInfo.CanonicalName()] = &AllocatableDevice{
			Vfio: vfioDeviceInfo,
		}
	}
	return devices, nil
}

// getVfioDeviceInfo 从PCI设备信息创建VFIO设备信息
// 参数:
//   - idx: 设备索引
//   - device: NVIDIA PCI设备信息
//
// 返回:
//   - *VfioDeviceInfo: VFIO设备信息
//   - error: 创建过程中的错误
//
// 设计说明:
// 1. 为什么使用SHA1生成UUID:
//   - VFIO设备没有硬件UUID(不像GPU有NVML UUID)
//   - 使用PCIe地址作为种子可以确保UUID的唯一性和可重现性
//   - 同一个PCI地址总是生成相同的UUID
//
// 2. 收集的信息包括:
//   - 识别信息: UUID, 设备ID, 厂商ID
//   - 拓扑信息: PCIe总线ID, NUMA节点, IOMMU组
//   - 资源信息: 可寻址内存大小
//
// 3. 这些信息对VFIO直通至关重要:
//   - IOMMU组决定了哪些设备必须一起直通
//   - NUMA节点影响内存访问性能
//   - 设备ID/厂商ID用于设备识别和驱动匹配
func (l deviceLib) getVfioDeviceInfo(idx int, device *nvpci.NvidiaPCIDevice) (*VfioDeviceInfo, error) {
	// 尝试获取PCIe根属性
	// 为什么可选: 不是所有系统都能提供PCIe拓扑信息，但仍可继续
	var pcieRootAttr *deviceattribute.DeviceAttribute
	attr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(device.Address)
	if err == nil {
		pcieRootAttr = &attr
	} else {
		// 警告但继续执行，没有这个信息不影响VFIO基本功能
		klog.Warningf("error getting PCIe root for device %s, continuing without attribute: %v", device.Address, err)
	}

	// 获取设备的可寻址内存总量
	// 参数true表示包含BAR(Base Address Register)区域
	// 为什么需要: 了解设备内存容量，用于资源分配决策
	_, memoryBytes := device.Resources.GetTotalAddressableMemory(true)

	// 组装VFIO设备信息
	vfioDeviceInfo := &VfioDeviceInfo{
		// 使用PCIe地址生成确定性的UUID
		// NameSpaceDNS确保跨系统的UUID一致性
		UUID:         uuid.NewSHA1(uuid.NameSpaceDNS, []byte(device.Address)).String(),
		index:        idx,
		productName:  device.DeviceName,
		pcieBusID:    device.Address,
		pcieRootAttr: pcieRootAttr,
		// 格式化为标准的十六进制表示(如"0x10de"表示NVIDIA)
		deviceID:               fmt.Sprintf("0x%04x", device.Device),
		vendorID:               fmt.Sprintf("0x%04x", device.Vendor),
		numaNode:               device.NumaNode,   // NUMA节点，用于优化内存访问
		iommuGroup:             device.IommuGroup, // IOMMU组，决定直通隔离边界
		addressableMemoryBytes: memoryBytes,
	}
	return vfioDeviceInfo, nil
}

// getMigDevices 获取GPU上的所有MIG设备
// 参数:
//   - gpuInfo: 父GPU信息
//
// 返回:
//   - map[string]*MigDeviceInfo: MIG设备映射(key是UUID)
//   - error: 获取过程中的错误
//
// 设计说明:
// 1. MIG设备的层次结构:
//   - GPU Instance (GI): 物理GPU的一个分区，有独立的内存和计算资源
//   - Compute Instance (CI): GI内的计算单元，定义了实际的计算能力
//   - MIG Device: GI和CI的组合，是实际可分配的资源单位
//
// 2. 为什么需要匹配profile:
//   - 需要知道MIG设备对应的是哪种配置(如1g.5gb, 3g.20gb等)
//   - profile信息决定了设备的属性和能力
//   - 用于DRA设备选择和资源匹配
//
// 3. 为什么复制父GPU的某些信息:
//   - MIG设备继承父GPU的PCIe位置和驱动信息
//   - 这些信息对于NUMA感知调度和设备访问很重要
func (l deviceLib) getMigDevices(gpuInfo *GpuInfo) (map[string]*MigDeviceInfo, error) {
	// 如果GPU未启用MIG，直接返回
	// 为什么检查: 避免不必要的NVML调用，提高性能
	if !gpuInfo.migEnabled {
		return nil, nil
	}

	// 初始化NVML以查询MIG设备
	if err := l.Init(); err != nil {
		return nil, err
	}
	defer l.alwaysShutdown()

	// 通过UUID获取GPU设备句柄
	// 为什么需要句柄: MIG设备查询需要通过父GPU句柄进行
	device, ret := l.nvmllib.DeviceGetHandleByUUID(gpuInfo.UUID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU device handle: %v", ret)
	}

	migInfos := make(map[string]*MigDeviceInfo)
	// 遍历所有MIG设备
	err := walkMigDevices(device, func(i int, migDevice nvml.Device) error {
		// 获取GPU Instance ID
		// 为什么需要: GI是MIG层次结构的顶层，需要它来查询详细信息
		giID, ret := migDevice.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance ID for MIG device: %v", ret)
		}

		// 通过ID获取GPU Instance句柄
		gi, ret := device.GetGpuInstanceById(giID)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance for '%v': %v", giID, ret)
		}

		// 获取GPU Instance的详细信息(内存大小、Profile ID、放置位置等)
		giInfo, ret := gi.GetInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance info for '%v': %v", giID, ret)
		}

		// 获取Compute Instance ID
		// 为什么需要: CI定义了MIG设备的实际计算能力
		ciID, ret := migDevice.GetComputeInstanceId()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance ID for MIG device: %v", ret)
		}

		// 通过ID获取Compute Instance句柄
		ci, ret := gi.GetComputeInstanceById(ciID)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance for '%v': %v", ciID, ret)
		}

		// 获取Compute Instance的详细信息(Profile ID、引擎数等)
		ciInfo, ret := ci.GetInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance info for '%v': %v", ciID, ret)
		}

		// 获取MIG设备的UUID
		// 为什么需要: UUID是MIG设备的唯一标识符，用于设备分配和跟踪
		uuid, ret := migDevice.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting UUID for MIG device: %v", ret)
		}

		// 在GPU支持的profile列表中查找匹配的profile
		// 为什么需要匹配: 需要知道这个MIG设备对应哪种配置(如"3g.20gb")
		var migProfile *MigProfileInfo
		var giProfileInfo *nvml.GpuInstanceProfileInfo
		var ciProfileInfo *nvml.ComputeInstanceProfileInfo
		for _, profile := range gpuInfo.migProfiles {
			profileInfo := profile.profile.GetInfo()
			// 获取GPU Instance profile信息并匹配
			gipInfo, ret := device.GetGpuInstanceProfileInfo(profileInfo.GIProfileID)
			if ret != nvml.SUCCESS {
				continue
			}
			// 检查GI的Profile ID是否匹配
			if giInfo.ProfileId != gipInfo.Id {
				continue
			}
			// 获取Compute Instance profile信息并匹配
			cipInfo, ret := gi.GetComputeInstanceProfileInfo(profileInfo.CIProfileID, profileInfo.CIEngProfileID)
			if ret != nvml.SUCCESS {
				continue
			}
			// 检查CI的Profile ID是否匹配
			if ciInfo.ProfileId != cipInfo.Id {
				continue
			}
			// 找到匹配的profile
			migProfile = profile
			giProfileInfo = &gipInfo
			ciProfileInfo = &cipInfo
		}
		// 如果找不到匹配的profile，这是一个错误
		// 为什么是错误: 每个MIG设备都应该对应一个已知的profile配置
		if migProfile == nil {
			return fmt.Errorf("error getting profile info for MIG device: %v", uuid)
		}

		// 记录MIG设备在GPU内存中的放置位置
		// 为什么需要: 放置信息用于避免资源碎片化和优化资源利用
		placement := MigDevicePlacement{
			GpuInstancePlacement: giInfo.Placement,
		}

		// 创建完整的MIG设备信息
		migInfos[uuid] = &MigDeviceInfo{
			UUID:          uuid,
			profile:       migProfile.String(), // profile名称(如"1g.5gb")
			parent:        gpuInfo,             // 关联父GPU
			placement:     &placement,
			giProfileInfo: giProfileInfo,
			giInfo:        &giInfo,
			ciProfileInfo: ciProfileInfo,
			ciInfo:        &ciInfo,
			pcieBusID:     gpuInfo.pcieBusID,    // 继承父GPU的PCIe信息
			pcieRootAttr:  gpuInfo.pcieRootAttr, // 继承父GPU的拓扑信息
			Health:        Healthy,              // 初始状态为健康
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error enumerating MIG devices: %w", err)
	}

	// 如果没有找到任何MIG设备，返回nil而不是空map
	// 为什么: 区分"查询成功但没有设备"和"查询失败"两种情况
	if len(migInfos) == 0 {
		return nil, nil
	}

	return migInfos, nil
}

// walkMigDevices 遍历GPU上的所有MIG设备
// 参数:
//   - d: GPU设备句柄
//   - f: 对每个MIG设备执行的回调函数
//
// 返回: 遍历过程中的错误
//
// 设计说明:
// 1. 为什么需要这个辅助函数:
//   - 封装MIG设备遍历的复杂性
//   - 处理NVML的各种错误情况
//   - 提供统一的遍历接口
//
// 2. 错误处理策略:
//   - NOT_FOUND和INVALID_ARGUMENT被视为正常情况(设备槽位未使用)
//   - 其他错误则导致遍历失败
//
// 3. 为什么遍历所有槽位:
//   - MIG设备可能在任何槽位
//   - 槽位之间可能有空隙(某些槽位未配置)
func walkMigDevices(d nvml.Device, f func(i int, d nvml.Device) error) error {
	// 获取GPU支持的最大MIG设备数量
	// 为什么需要: 确定遍历的范围
	count, ret := nvml.Device(d).GetMaxMigDeviceCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting max MIG device count: %v", ret)
	}

	// 遍历所有可能的MIG设备槽位
	for i := 0; i < count; i++ {
		// 尝试获取该索引位置的MIG设备句柄
		device, ret := d.GetMigDeviceHandleByIndex(i)
		if ret == nvml.ERROR_NOT_FOUND {
			// 该槽位没有MIG设备，跳过
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			// 无效的索引，跳过
			continue
		}
		if ret != nvml.SUCCESS {
			// 其他错误视为失败
			return fmt.Errorf("error getting MIG device handle at index '%v': %v", i, ret)
		}
		// 对找到的MIG设备执行回调函数
		err := f(i, device)
		if err != nil {
			return err
		}
	}
	return nil
}

// setTimeSlice 为指定的GPU设备设置时间片(time slice)
// 参数:
//   - uuids: GPU UUID列表
//   - timeSlice: 时间片值(微秒)
//
// 返回: 设置过程中的错误
//
// 设计说明:
// 1. 什么是时间片:
//   - 时间片控制GPU在多个进程/容器间的上下文切换频率
//   - 较小的时间片提供更好的响应性但可能降低吞吐量
//   - 较大的时间片提高吞吐量但可能增加延迟
//
// 2. 为什么使用nvidia-smi而不是NVML API:
//   - NVML没有提供设置计算策略(compute policy)的API
//   - nvidia-smi是NVIDIA官方推荐的配置工具
//   - nvidia-smi内部会处理必要的权限和验证
//
// 3. 为什么需要设置LD_PRELOAD:
//   - nvidia-smi依赖libnvidia-ml.so.1
//   - 在容器环境中，库可能不在标准搜索路径
//   - 显式指定库路径确保nvidia-smi能正常运行
//
// 4. 使用场景:
//   - 共享GPU场景中优化多租户性能
//   - 根据工作负载特性调整调度策略
func (l deviceLib) setTimeSlice(uuids []string, timeSlice int) error {
	// 为每个GPU设备单独设置时间片
	// 为什么逐个设置: nvidia-smi命令一次只能操作一个设备
	for _, uuid := range uuids {
		// 构造nvidia-smi命令
		// compute-policy: 计算策略子命令
		// -i: 指定设备UUID
		// --set-timeslice: 设置时间片值
		cmd := exec.Command(
			l.nvidiaSMIPath,
			"compute-policy",
			"-i", uuid,
			"--set-timeslice", fmt.Sprintf("%d", timeSlice))

		// In order for nvidia-smi to run, we need update LD_PRELOAD to include the path to libnvidia-ml.so.1.
		// 设置LD_PRELOAD环境变量，确保nvidia-smi能找到驱动库
		// 为什么这样做:
		// 1. 容器环境中库路径可能非标准
		// 2. 确保加载正确版本的驱动库
		// 3. 避免与系统中其他版本的冲突
		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))

		// 执行命令并捕获输出
		// 为什么用CombinedOutput: 同时捕获stdout和stderr，便于错误诊断
		output, err := cmd.CombinedOutput()
		if err != nil {
			// 记录完整的命令输出以便调试
			klog.Errorf("\n%v", string(output))
			return fmt.Errorf("error running nvidia-smi: %w", err)
		}
	}
	return nil
}

// setComputeMode 为指定的GPU设备设置计算模式
// 参数:
//   - uuids: GPU UUID列表
//   - mode: 计算模式(如"DEFAULT", "EXCLUSIVE_PROCESS", "PROHIBITED")
//
// 返回: 设置过程中的错误
//
// 设计说明:
// 1. 计算模式的含义:
//   - DEFAULT: 多个进程可以同时使用GPU(默认)
//   - EXCLUSIVE_PROCESS: 同一时间只有一个进程可以使用GPU
//   - EXCLUSIVE_THREAD: 同一时间只有一个线程可以使用GPU(已废弃)
//   - PROHIBITED: 禁止所有计算任务
//
// 2. 为什么需要设置计算模式:
//   - 提供GPU的独占访问(用于需要完全控制GPU的工作负载)
//   - 防止资源争用和性能干扰
//   - 支持不同的安全和隔离需求
//
// 3. 为什么使用nvidia-smi:
//   - 与setTimeSlice相同的原因: NVML没有设置计算模式的API
//   - nvidia-smi提供统一的配置接口
//
// 4. 使用场景:
//   - 独占GPU分配(一个容器独占整个GPU)
//   - 多租户隔离
//   - 特殊工作负载要求(如MPS - Multi-Process Service)
func (l deviceLib) setComputeMode(uuids []string, mode string) error {
	// 为每个GPU设备单独设置计算模式
	for _, uuid := range uuids {
		// 构造nvidia-smi命令
		// -i: 指定设备UUID
		// -c: 设置计算模式(compute mode)
		cmd := exec.Command(
			l.nvidiaSMIPath,
			"-i", uuid,
			"-c", mode)

		// In order for nvidia-smi to run, we need update LD_PRELOAD to include the path to libnvidia-ml.so.1.
		// 设置LD_PRELOAD环境变量以确保nvidia-smi能找到驱动库
		// 这对于容器环境中的可靠执行至关重要
		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))

		// 执行命令并捕获输出
		output, err := cmd.CombinedOutput()
		if err != nil {
			// 记录错误输出以便诊断问题
			// 常见错误包括: 无效的模式值、权限不足、设备繁忙等
			klog.Errorf("\n%v", string(output))
			return fmt.Errorf("error running nvidia-smi: %w", err)
		}
	}
	return nil
}

// TODO: Reenable dynamic MIG functionality once it is supported in Kubernetes 1.32
//
// func (l deviceLib) createMigDevice(gpu *GpuInfo, profile nvdev.MigProfile, placement *nvml.GpuInstancePlacement) (*MigDeviceInfo, error) {
// 	if err := l.Init(); err != nil {
// 		return nil, err
// 	}
// 	defer l.alwaysShutdown()
//
// 	profileInfo := profile.GetInfo()
//
// 	device, ret := l.nvmllib.DeviceGetHandleByUUID(gpu.UUID)
// 	if ret != nvml.SUCCESS {
// 		return nil, fmt.Errorf("error getting GPU device handle: %v", ret)
// 	}
//
// 	giProfileInfo, ret := device.GetGpuInstanceProfileInfo(profileInfo.GIProfileID)
// 	if ret != nvml.SUCCESS {
// 		return nil, fmt.Errorf("error getting GPU instance profile info for '%v': %v", profile, ret)
// 	}
//
// 	gi, ret := device.CreateGpuInstanceWithPlacement(&giProfileInfo, placement)
// 	if ret != nvml.SUCCESS {
// 		return nil, fmt.Errorf("error creating GPU instance for '%v': %v", profile, ret)
// 	}
//
// 	giInfo, ret := gi.GetInfo()
// 	if ret != nvml.SUCCESS {
// 		return nil, fmt.Errorf("error getting GPU instance info for '%v': %v", profile, ret)
// 	}
//
// 	ciProfileInfo, ret := gi.GetComputeInstanceProfileInfo(profileInfo.CIProfileID, profileInfo.CIEngProfileID)
// 	if ret != nvml.SUCCESS {
// 		return nil, fmt.Errorf("error getting Compute instance profile info for '%v': %v", profile, ret)
// 	}
//
// 	ci, ret := gi.CreateComputeInstance(&ciProfileInfo)
// 	if ret != nvml.SUCCESS {
// 		return nil, fmt.Errorf("error creating Compute instance for '%v': %v", profile, ret)
// 	}
//
// 	ciInfo, ret := ci.GetInfo()
// 	if ret != nvml.SUCCESS {
// 		return nil, fmt.Errorf("error getting GPU instance info for '%v': %v", profile, ret)
// 	}
//
// 	uuid := ""
// 	err := walkMigDevices(device, func(i int, migDevice nvml.Device) error {
// 		giID, ret := migDevice.GetGpuInstanceId()
// 		if ret != nvml.SUCCESS {
// 			return fmt.Errorf("error getting GPU instance ID for MIG device: %v", ret)
// 		}
// 		ciID, ret := migDevice.GetComputeInstanceId()
// 		if ret != nvml.SUCCESS {
// 			return fmt.Errorf("error getting Compute instance ID for MIG device: %v", ret)
// 		}
// 		if giID != int(giInfo.Id) || ciID != int(ciInfo.Id) {
// 			return nil
// 		}
// 		uuid, ret = migDevice.GetUUID()
// 		if ret != nvml.SUCCESS {
// 			return fmt.Errorf("error getting UUID for MIG device: %v", ret)
// 		}
// 		return nil
// 	})
// 	if err != nil {
// 		return nil, fmt.Errorf("error processing MIG device for GI and CI just created: %w", err)
// 	}
// 	if uuid == "" {
// 		return nil, fmt.Errorf("unable to find MIG device for GI and CI just created")
// 	}
//
// 	migInfo := &MigDeviceInfo{
// 		UUID:    uuid,
// 		parent:  gpu,
// 		profile: profile,
// 		giInfo:  &giInfo,
// 		ciInfo:  &ciInfo,
// 	}
//
// 	return migInfo, nil
// }
//
// func (l deviceLib) deleteMigDevice(mig *MigDeviceInfo) error {
// 	if err := l.Init(); err != nil {
// 		return err
// 	}
// 	defer l.alwaysShutdown()
//
// 	parent, ret := l.nvmllib.DeviceGetHandleByUUID(mig.parent.UUID)
// 	if ret != nvml.SUCCESS {
// 		return fmt.Errorf("error getting device from UUID '%v': %v", mig.parent.UUID, ret)
// 	}
// 	gi, ret := parent.GetGpuInstanceById(int(mig.giInfo.Id))
// 	if ret != nvml.SUCCESS {
// 		return fmt.Errorf("error getting GPU instance ID for MIG device: %v", ret)
// 	}
// 	ci, ret := gi.GetComputeInstanceById(int(mig.ciInfo.Id))
// 	if ret != nvml.SUCCESS {
// 		return fmt.Errorf("error getting Compute instance ID for MIG device: %v", ret)
// 	}
// 	ret = ci.Destroy()
// 	if ret != nvml.SUCCESS {
// 		return fmt.Errorf("error destroying Compute Instance: %v", ret)
// 	}
// 	ret = gi.Destroy()
// 	if ret != nvml.SUCCESS {
// 		return fmt.Errorf("error destroying GPU Instance: %v", ret)
// 	}
// 	return nil
// }
