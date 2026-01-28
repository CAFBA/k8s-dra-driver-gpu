/*
 * Copyright (c) 2022-2023, NVIDIA CORPORATION.  All rights reserved.
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
	"io"

	"github.com/sirupsen/logrus"

	nvdevice "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/spec"
	transformroot "github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/transform/root"
	"k8s.io/klog/v2"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/NVIDIA/k8s-dra-driver-gpu/internal/common"
	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/featuregates"
)

const (
	// cdiVendor 定义 CDI 供应商名称
	// 使用 "k8s." 前缀表示这是 Kubernetes 集成的驱动
	// 完整的供应商名称将是 "k8s.gpu.nvidia.com"
	cdiVendor = "k8s." + DriverName

	// cdiDeviceClass 和 cdiDeviceKind 定义标准设备类
	// 标准设备类用于发布所有可分配设备的基本 CDI 规范
	// 格式：k8s.gpu.nvidia.com/device=<device-name>
	cdiDeviceClass = "device"
	cdiDeviceKind  = cdiVendor + "/" + cdiDeviceClass

	// cdiClaimClass 和 cdiClaimKind 定义 claim 特定的设备类
	// claim 类用于发布特定于某个 ResourceClaim 的配置（如 MPS、时间片）
	// 格式：k8s.gpu.nvidia.com/claim=<claim-uid>-<device-name>
	cdiClaimClass = "claim"
	cdiClaimKind  = cdiVendor + "/" + cdiClaimClass

	// cdiBaseSpecIdentifier 用于标识基础 NVIDIA 设备规范文件
	// 这个规范包含所有通过 nvidia 驱动管理的设备（GPU 和 MIG）
	cdiBaseSpecIdentifier = "base"

	// cdiVfioSpecIdentifier 用于标识 VFIO 设备规范文件
	// VFIO 设备有完全不同的注入机制，因此需要单独的规范文件
	cdiVfioSpecIdentifier = "vfio"

	// defaultCDIRoot 是 CDI 规范文件的默认存储路径
	// 容器运行时会从这个目录读取 CDI 规范
	defaultCDIRoot = "/var/run/cdi"

	// procNvCapsPath 是 NVIDIA capabilities 文件系统的路径
	// 用于 MIG 设备访问权限的 capabilities 文件
	// 格式：/proc/driver/nvidia/capabilities/gpu<N>/mig/gi<X>/ci<Y>/access
	procNvCapsPath = "/proc/driver/nvidia/capabilities"
)

// CDIHandler 负责生成和管理 CDI 规范文件
// CDI（Container Device Interface）是一个标准化的接口，用于向容器注入设备
// 它取代了各运行时特定的设备注入机制（如 Docker 的 --device 标志）
type CDIHandler struct {
	logger            *logrus.Logger     // 日志记录器（通常禁用以避免重复日志）
	nvml              nvml.Interface     // NVML 库接口，用于查询 GPU 信息
	nvdevice          nvdevice.Interface // go-nvlib 设备接口，NVML 的高级封装
	nvcdiDevice       nvcdi.Interface    // NVIDIA CDI 库（设备类）
	nvcdiClaim        nvcdi.Interface    // NVIDIA CDI 库（claim 类）
	cache             *cdiapi.Cache      // CDI 规范缓存，管理磁盘上的规范文件
	driverRoot        string             // 容器内的驱动根路径（如 /）
	devRoot           string             // 设备节点根路径（如 /dev）
	targetDriverRoot  string             // 宿主机上的驱动根路径（用于路径转换）
	nvidiaCDIHookPath string             // NVIDIA CDI hook 可执行文件路径

	cdiRoot     string // CDI 规范文件存储目录
	vendor      string // CDI 供应商名称
	deviceClass string // 设备类名称
	claimClass  string // claim 类名称
}

// NewCDIHandler 创建一个新的 CDI 处理器
// 使用 functional options 模式允许灵活的配置
// 这种模式的优势：
// 1. 可选参数无需定义多个构造函数
// 2. 参数顺序无关紧要
// 3. 易于添加新的配置选项而不破坏现有代码
func NewCDIHandler(opts ...cdiOption) (*CDIHandler, error) {
	h := &CDIHandler{}
	// 应用所有配置选项
	for _, opt := range opts {
		opt(h)
	}

	// 设置默认值
	if h.logger == nil {
		// 创建一个静默日志记录器
		// 因为 klog 已经提供日志功能，避免重复日志输出
		h.logger = logrus.New()
		h.logger.SetOutput(io.Discard)
	}
	if h.nvml == nil {
		h.nvml = nvml.New()
	}
	if h.cdiRoot == "" {
		h.cdiRoot = defaultCDIRoot
	}
	if h.nvdevice == nil {
		h.nvdevice = nvdevice.New(h.nvml)
	}
	if h.vendor == "" {
		h.vendor = cdiVendor
	}
	if h.deviceClass == "" {
		h.deviceClass = cdiDeviceClass
	}
	if h.claimClass == "" {
		h.claimClass = cdiClaimClass
	}

	// 创建用于标准设备的 NVIDIA CDI 库实例
	// 这个实例负责生成 GPU 和 MIG 设备的 CDI 规范
	if h.nvcdiDevice == nil {
		nvcdilib, err := nvcdi.New(
			nvcdi.WithDeviceLib(h.nvdevice),
			nvcdi.WithDriverRoot(h.driverRoot),
			nvcdi.WithDevRoot(h.devRoot),
			nvcdi.WithLogger(h.logger),
			nvcdi.WithNvmlLib(h.nvml),
			nvcdi.WithMode("nvml"), // 使用 NVML 模式查询设备信息
			nvcdi.WithVendor(h.vendor),
			nvcdi.WithClass(h.deviceClass),
			nvcdi.WithNVIDIACDIHookPath(h.nvidiaCDIHookPath),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create CDI library for devices: %w", err)
		}
		h.nvcdiDevice = nvcdilib
	}

	// 创建用于 claim 特定配置的 NVIDIA CDI 库实例
	// 这个实例负责生成包含 MPS/时间片配置的 CDI 规范
	if h.nvcdiClaim == nil {
		nvcdilib, err := nvcdi.New(
			nvcdi.WithDeviceLib(h.nvdevice),
			nvcdi.WithDriverRoot(h.driverRoot),
			nvcdi.WithDevRoot(h.devRoot),
			nvcdi.WithLogger(h.logger),
			nvcdi.WithNvmlLib(h.nvml),
			nvcdi.WithMode("nvml"),
			nvcdi.WithVendor(h.vendor),
			nvcdi.WithClass(h.claimClass), // 使用不同的类名区分设备和 claim
			nvcdi.WithNVIDIACDIHookPath(h.nvidiaCDIHookPath),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create CDI library for claims: %w", err)
		}
		h.nvcdiClaim = nvcdilib
	}

	// 创建 CDI 规范缓存
	// 缓存负责：
	// 1. 管理规范文件的读写
	// 2. 验证规范格式
	// 3. 提供规范查询接口
	if h.cache == nil {
		cache, err := cdiapi.NewCache(
			cdiapi.WithSpecDirs(h.cdiRoot),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create a new CDI cache: %w", err)
		}
		h.cache = cache
	}

	return h, nil
}

// writeSpec 将 CDI 规范写入磁盘
// 执行以下转换：
// 1. 路径转换：将容器内路径转换为宿主机路径
// 2. 版本优化：使用最小必需的 CDI 规范版本
// 3. 持久化：写入缓存管理的规范目录
func (cdi *CDIHandler) writeSpec(spec spec.Interface, specName string) error {
	// Transform the spec to make it aware that it is running inside a container.
	// 转换规范以感知容器化环境
	// 场景：驱动插件在容器中运行，但需要引用宿主机上的驱动文件
	// 例如：容器内的 /host/usr/lib 对应宿主机的 /usr/lib
	err := transformroot.New(
		transformroot.WithRoot(cdi.driverRoot),             // 容器内的根路径
		transformroot.WithTargetRoot(cdi.targetDriverRoot), // 宿主机的根路径
		transformroot.WithRelativeTo("host"),               // 相对于宿主机进行转换
	).Transform(spec.Raw())
	if err != nil {
		return fmt.Errorf("failed to transform driver root in CDI spec: %w", err)
	}

	// Update the spec to include only the minimum version necessary.
	// 确定并设置最小必需的 CDI 规范版本
	// 为什么重要：
	// 1. 向后兼容：旧版运行时可能不支持新版本特性
	// 2. 最大兼容性：使用最低版本增加运行时兼容性
	// 3. 验证：确保规范中的特性在该版本中受支持
	minVersion, err := cdispec.MinimumRequiredVersion(spec.Raw())
	if err != nil {
		return fmt.Errorf("failed to get minimum required CDI spec version: %w", err)
	}
	spec.Raw().Version = minVersion

	// Write the spec out to disk.
	// 将规范写入磁盘
	// 缓存会处理文件命名、原子写入和权限设置
	return cdi.cache.WriteSpec(spec.Raw(), specName)

}

// CreateStandardDeviceSpecFile 创建标准设备的 CDI 规范文件
// "标准设备"是指所有可分配的设备，不包含任何 claim 特定的配置
// 这些规范在驱动启动时创建一次，并在设备健康状态变化时更新
func (cdi *CDIHandler) CreateStandardDeviceSpecFile(allocatable AllocatableDevices) error {
	// 创建 NVIDIA GPU 和 MIG 设备的规范
	if err := cdi.createStandardNvidiaDeviceSpecFile(allocatable); err != nil {
		klog.Errorf("failed to create standard nvidia device spec file: %v", err)
		return err
	}

	// 如果启用了 VFIO 直通支持，创建 VFIO 设备的规范
	// VFIO 设备需要完全不同的设备注入机制
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		if err := cdi.createStandardVfioDeviceSpecFile(allocatable); err != nil {
			klog.Errorf("failed to create standard vfio device spec file: %v", err)
			return err
		}
	}
	return nil
}

// createStandardVfioDeviceSpecFile 创建 VFIO 设备的 CDI 规范
// VFIO（Virtual Function I/O）设备通过 vfio-pci 驱动直接分配给容器
func (cdi *CDIHandler) createStandardVfioDeviceSpecFile(allocatable AllocatableDevices) error {
	// 获取所有 VFIO 设备通用的容器编辑
	// 通用编辑包括：
	// 1. /dev/vfio/vfio 字符设备（VFIO 容器）
	// 2. 必要的环境变量
	// 3. 权限和能力
	commonEdits := GetVfioCommonCDIContainerEdits()

	var deviceSpecs []cdispec.Device
	// 为每个 VFIO 设备生成规范
	for _, device := range allocatable {
		if device.Type() != VfioDeviceType {
			continue
		}
		// 获取设备特定的容器编辑
		// 设备特定编辑包括：/dev/vfio/<iommu-group> 字符设备
		edits := GetVfioCDIContainerEdits(device.Vfio)
		dspec := cdispec.Device{
			Name:           device.CanonicalName(),
			ContainerEdits: *edits.ContainerEdits,
		}
		deviceSpecs = append(deviceSpecs, dspec)
	}

	// 如果没有 VFIO 设备，不创建规范文件
	if len(deviceSpecs) == 0 {
		return nil
	}

	// 创建 CDI 规范对象
	spec, err := spec.New(
		spec.WithVendor(cdiVendor),
		spec.WithClass(cdiDeviceClass),
		spec.WithDeviceSpecs(deviceSpecs),
		spec.WithEdits(*commonEdits.ContainerEdits), // 应用通用编辑
	)
	if err != nil {
		return fmt.Errorf("failed to creat CDI spec: %w", err)
	}

	// 生成临时规范文件名
	// 格式：<vendor>-<class>-<identifier>.yaml
	// 例如：k8s.gpu.nvidia.com-device-vfio.yaml
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiDeviceClass, cdiVfioSpecIdentifier)
	klog.Infof("Writing vfio spec for %s to %s", specName, cdi.cdiRoot)
	return cdi.writeSpec(spec, specName)
}

// createStandardNvidiaDeviceSpecFile 创建 NVIDIA GPU 和 MIG 设备的 CDI 规范
// 这是最复杂的规范生成逻辑，因为它需要处理：
// 1. 完整 GPU 设备
// 2. MIG 设备（需要额外的 capabilities 设备节点）
// 3. NVIDIA 运行时集成（避免双重注入）
func (cdi *CDIHandler) createStandardNvidiaDeviceSpecFile(allocatable AllocatableDevices) error {
	// Initialize NVML in order to get the device edits.
	// 初始化 NVML 以查询设备信息
	// NVML 需要显式初始化和关闭，不能保持长期运行
	if r := cdi.nvml.Init(); r != nvml.SUCCESS {
		return fmt.Errorf("failed to initialize NVML: %v", r)
	}
	defer func() {
		if r := cdi.nvml.Shutdown(); r != nvml.SUCCESS {
			klog.Warningf("failed to shutdown NVML: %v", r)
		}
	}()

	// Generate the set of common edits.
	// 生成所有设备共享的容器编辑
	// 通用编辑包括：
	// 1. NVIDIA 驱动库（libcuda.so、libnvidia-ml.so 等）
	// 2. 设备节点（/dev/nvidia-uvm、/dev/nvidiactl）
	// 3. 必要的环境变量
	commonEdits, err := cdi.nvcdiDevice.GetCommonEdits()
	if err != nil {
		return fmt.Errorf("failed to get common CDI spec edits: %w", err)
	}

	// Make sure that NVIDIA_VISIBLE_DEVICES is set to void to avoid the
	// nvidia-container-runtime honoring it in addition to the underlying
	// runtime honoring CDI.
	// 设置 NVIDIA_VISIBLE_DEVICES=void
	// 关键原因：
	// 1. 避免双重设备注入：nvidia-container-runtime 会处理这个环境变量
	// 2. CDI 是新的标准方式，应该独占设备注入
	// 3. "void" 告诉 nvidia-container-runtime 不要注入任何设备
	commonEdits.Env = append(
		commonEdits.Env,
		"NVIDIA_VISIBLE_DEVICES=void")

	// Generate device specs for all full GPUs and MIG devices.
	// 为所有完整 GPU 和 MIG 设备生成规范
	var deviceSpecs []cdispec.Device
	for _, device := range allocatable {
		// 跳过 VFIO 设备（它们在单独的规范文件中）
		if device.Type() == VfioDeviceType {
			continue
		}

		uuid := device.UUID()
		if device.Type() == MigDeviceType {
			// Goal: inject parent dev node. Other dev nodes specific to this
			// MIG device are injected 'manually' further below. That is because
			// currently `nvcdiDevice.GetDeviceSpecsByID()` may yield an
			// incomplete spec for MIG devices, see
			// https://github.com/NVIDIA/k8s-dra-driver-gpu/issues/787. Instead,
			// manually create the required dev node spec for the consuming
			// container via GetDevNodesForMigDevice() below.
			// 对于 MIG 设备，使用父 GPU 的 UUID
			// 原因：
			// 1. GetDeviceSpecsByID 主要返回父 GPU 设备节点（/dev/nvidia<N>）
			// 2. MIG 特定的 capabilities 设备节点需要手动添加
			// 3. 这是一个已知限制，见 issue #787
			uuid = device.Mig.parent.UUID
		}

		// 获取设备的 CDI 规范
		// 这包括设备节点、环境变量和挂载点
		dspecs, err := cdi.nvcdiDevice.GetDeviceSpecsByID(uuid)
		if err != nil {
			return fmt.Errorf("unable to get device spec for %s: %w", device.CanonicalName(), err)
		}
		// 使用我们的规范名称覆盖默认名称
		// 这确保名称与 ResourceSlice 中的设备名称一致
		dspecs[0].Name = device.CanonicalName()

		if device.Type() == MigDeviceType {
			// 为 MIG 设备手动添加 capabilities 设备节点
			// MIG 设备需要两个额外的字符设备：
			// 1. /dev/nvidia-caps/nvidia-cap<GIm> (GPU Instance capability)
			// 2. /dev/nvidia-caps/nvidia-cap<CIm> (Compute Instance capability)
			devnodesForMig, err := cdi.GetDevNodesForMigDevice(device.Mig.parent.minor, int(device.Mig.giInfo.Id), int(device.Mig.ciInfo.Id))
			if err != nil {
				return fmt.Errorf("failed to construct MIG device DeviceNode edits: %w", err)
			}
			klog.V(7).Infof("CDI spec: appending MIG device nodes")
			dspecs[0].ContainerEdits.DeviceNodes = append(dspecs[0].ContainerEdits.DeviceNodes, devnodesForMig...)
		}

		deviceSpecs = append(deviceSpecs, dspecs[0])
	}

	// Generate base spec from commonEdits and deviceEdits.
	// 从通用编辑和设备编辑生成基础规范
	spec, err := spec.New(
		spec.WithVendor(cdiVendor),
		spec.WithClass(cdiDeviceClass),
		spec.WithDeviceSpecs(deviceSpecs),
		spec.WithEdits(*commonEdits.ContainerEdits),
	)
	if err != nil {
		return fmt.Errorf("failed to creat CDI spec: %w", err)
	}

	// 生成规范文件名并写入磁盘
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiDeviceClass, cdiBaseSpecIdentifier)
	klog.Infof("Writing spec for %s to %s", specName, cdi.cdiRoot)
	return cdi.writeSpec(spec, specName)
}

// CreateClaimSpecFile 为特定的 ResourceClaim 创建 CDI 规范
// Claim 规范包含特定于该 claim 的配置，例如：
// 1. MPS 环境变量（CUDA_MPS_PIPE_DIRECTORY）
// 2. 时间片设置
// 3. 其他 claim 特定的容器编辑
func (cdi *CDIHandler) CreateClaimSpecFile(claimUID string, preparedDevices PreparedDevices) error {
	// Generate claim specific specs for each device.
	// 为每个设备生成 claim 特定的规范
	var deviceSpecs []cdispec.Device
	for _, group := range preparedDevices {
		// If there are no edits passed back as part of the device config state, skip it
		// 如果设备配置状态中没有容器编辑，跳过
		// 这发生在设备不需要特殊配置时（如标准的独占 GPU）
		if group.ConfigState.containerEdits == nil {
			continue
		}

		// Apply any edits passed back as part of the device config state to all devices
		// 将设备配置状态中的容器编辑应用到所有设备
		// 同一组中的所有设备共享相同的配置（如同一个 MPS 守护进程）
		for _, device := range group.Devices {
			deviceSpec := cdispec.Device{
				// 设备名称格式：<claim-uid>-<device-canonical-name>
				// 例如：abc123-gpu-0-GPU-12345678
				Name:           fmt.Sprintf("%s-%s", claimUID, device.CanonicalName()),
				ContainerEdits: *group.ConfigState.containerEdits.ContainerEdits,
			}

			deviceSpecs = append(deviceSpecs, deviceSpec)
		}
	}

	// If there are no claim specific deviceSpecs, just return without creating the spec file
	// 如果没有 claim 特定的设备规范，不创建规范文件
	// 这是正常情况，大多数简单的设备分配不需要 claim 规范
	if len(deviceSpecs) == 0 {
		return nil
	}

	// Generate the claim specific device spec for this driver.
	// 生成此驱动的 claim 特定设备规范
	spec, err := spec.New(
		spec.WithVendor(cdiVendor),
		spec.WithClass(cdiClaimClass), // 使用 claim 类而不是 device 类
		spec.WithDeviceSpecs(deviceSpecs),
	)
	if err != nil {
		return fmt.Errorf("failed to creat CDI spec: %w", err)
	}

	// Write the spec out to disk.
	// 将规范写入磁盘
	// 规范文件名包含 claim UID，确保每个 claim 有独立的规范文件
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	klog.Infof("Writing claim spec for %s to %s", specName, cdi.cdiRoot)
	return cdi.writeSpec(spec, specName)
}

// DeleteClaimSpecFile 删除 ResourceClaim 的 CDI 规范文件
// 在 unprepare 期间调用，清理不再需要的配置
func (cdi *CDIHandler) DeleteClaimSpecFile(claimUID string) error {
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	return cdi.cache.RemoveSpec(specName)
}

// GetStandardDevice 返回设备的标准 CDI 设备 ID
// 格式：<vendor>/<class>=<device-name>
// 例如：k8s.gpu.nvidia.com/device=gpu-0-GPU-12345678
// 这个 ID 用于容器运行时查找和注入设备
func (cdi *CDIHandler) GetStandardDevice(device *AllocatableDevice) string {
	return cdiparser.QualifiedName(cdiVendor, cdiDeviceClass, device.CanonicalName())
}

// GetClaimDevice 返回 claim 特定的 CDI 设备 ID
// 格式：<vendor>/<class>=<claim-uid>-<device-name>
// 例如：k8s.gpu.nvidia.com/claim=abc123-gpu-0-GPU-12345678
// 如果没有容器编辑（不需要 claim 规范），返回空字符串
func (cdi *CDIHandler) GetClaimDevice(claimUID string, device *AllocatableDevice, containerEdits *cdiapi.ContainerEdits) string {
	if containerEdits == nil {
		return ""
	}
	return cdiparser.QualifiedName(cdiVendor, cdiClaimClass, fmt.Sprintf("%s-%s", claimUID, device.CanonicalName()))
}

// GetDevNodesForMigDevice 构造并返回 MIG 设备的 CDI 设备节点规范
// 为特定 MIG 设备的两个字符设备构造规范：
// 1. /dev/nvidia-caps/nvidia-cap<CIm>
// 2. /dev/nvidia-caps/nvidia-cap<GIm>
//
// Construct and return the CDI `deviceNodes` specification for the two
// character devices `/dev/nvidia-caps/nvidia-cap<CIm>` and
// `/dev/nvidia-caps/nvidia-cap<GIm>` for a specific MIG device.
//
// Context: for containerized workload to see and use a specific MIG device, it
// needs to be able to open three character device nodes:
// 上下文：容器化工作负载要使用特定 MIG 设备，需要打开三个字符设备节点：
//
// 1) `/dev/nvidia<Pm>`, with <Pm> referring to the parent's minor. This exists
// on the host.
// 1) /dev/nvidia<Pm>：<Pm> 是父 GPU 的 minor 号。这个设备在宿主机上已存在。
//
// 2) /dev/nvidia-caps/nvidia-cap<CIm> and /dev/nvidia-caps/nvidia-cap<GIm>,
// with <GIm> and <CIm> referring to the MIG GPU instance's and Compute
// instance's minor, respectively. For the the latter two device nodes it is
// sufficient to create them in the container (with proper cgroups permissions),
// without actually requiring the same device nodes to be explicitly created on
// the host. That is what is achieved below with the structure created in
// cdiDevNodeFromNVCapDevInfo().
//  2. /dev/nvidia-caps/nvidia-cap<GIm> 和 /dev/nvidia-caps/nvidia-cap<CIm>：
//     <GIm> 和 <CIm> 分别指 MIG GPU 实例和计算实例的 minor 号。
//     对于后两个设备节点，只需在容器中创建它们（带有正确的 cgroups 权限），
//     不需要在宿主机上显式创建。这就是下面通过 cdiDevNodeFromNVCapDevInfo()
//     创建的结构所实现的。
func (cdi *CDIHandler) GetDevNodesForMigDevice(parentMinor int, giId int, ciId int) ([]*cdispec.DeviceNode, error) {
	// 构造 GPU Instance 和 Compute Instance 的 capabilities 文件路径
	// 这些文件在 /proc 文件系统中，包含设备的主次设备号
	// 格式：/proc/driver/nvidia/capabilities/gpu<N>/mig/gi<X>/access
	//      /proc/driver/nvidia/capabilities/gpu<N>/mig/gi<X>/ci<Y>/access
	gipath := fmt.Sprintf("%s/gpu%d/mig/gi%d/access", procNvCapsPath, parentMinor, giId)
	cipath := fmt.Sprintf("%s/gpu%d/mig/gi%d/ci%d/access", procNvCapsPath, parentMinor, giId, ciId)

	// 解析 GPU Instance capabilities 文件
	// 这个文件包含设备的主次设备号，用于创建设备节点
	giCapsInfo, err := common.ParseNVCapDeviceInfo(gipath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse GI capabilities file %s: %w", gipath, err)
	}

	// 解析 Compute Instance capabilities 文件
	ciCapsInfo, err := common.ParseNVCapDeviceInfo(cipath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CI capabilities file %s: %w", cipath, err)
	}

	// 创建 CDI 设备节点规范
	// CDICharDevNode() 将 capabilities 信息转换为 CDI 设备节点结构
	// 包含路径、主次设备号和权限
	devnodes := []*cdispec.DeviceNode{giCapsInfo.CDICharDevNode(), ciCapsInfo.CDICharDevNode()}
	return devnodes, nil
}
