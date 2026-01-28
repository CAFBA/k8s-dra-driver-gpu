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

// ============================================================================
// device_state.go - 设备状态管理
// - driver.go 处理与 kubelet 的通信和协议层面
// - device_state.go 处理实际的设备操作和状态管理
// ============================================================================
// 1. 管理可分配设备列表 (allocatable)
// 2. 处理设备准备 (Prepare) 和清理 (Unprepare) 的核心逻辑
// 3. 管理 checkpoint（用于崩溃恢复）
// 4. 协调各种设备管理器（CDI、时间片、MPS、VFIO）
// ============================================================================

package main

import (
	"context"
	"fmt"
	"slices"
	"sync"

	// resourceapi 是 Kubernetes DRA 的核心 API 类型
	// ResourceClaim 是用户请求资源的声明
	resourceapi "k8s.io/api/resource/v1"

	// runtime 包用于处理 Kubernetes 运行时对象
	// 这里用于处理 opaque 配置的解码
	"k8s.io/apimachinery/pkg/runtime"

	// kubeletplugin 提供 DRA 插件相关的类型
	// Device 类型表示准备好的设备
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"k8s.io/klog/v2"

	// checkpointmanager 是 kubelet 的检查点管理器
	// 用于持久化状态，支持崩溃后恢复
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	// cdiapi 是 Container Device Interface 的 API
	// CDI 是容器设备接口标准，定义如何将设备暴露给容器
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"

	// configapi 是 NVIDIA 驱动的配置 API
	// 定义 GpuConfig、MigDeviceConfig 等配置类型
	configapi "github.com/NVIDIA/k8s-dra-driver-gpu/api/nvidia.com/resource/v1beta1"

	// featuregates 用于特性门控
	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/featuregates"
)

// OpaqueDeviceConfig 表示一个不透明的设备配置，即 YAML 中的 spec.devices.config
// "不透明"意味着这个配置对 Kubernetes 调度器是不可见的，调度器不理解这些配置的内容，只是传递给驱动处理
type OpaqueDeviceConfig struct {
	// Requests 指定这个配置适用于哪些请求
	// 如果为空，表示这是默认配置，适用于所有未明确指定的请求
	// 例如：["gpu-1", "gpu-2"] 表示只适用于这两个请求
	Requests []string

	// Config 是实际的配置对象，可以是 *GpuConfig、*MigDeviceConfig 或 *VfioDeviceConfig
	// 使用 runtime.Object 接口允许存储不同类型的配置
	Config runtime.Object
}

// DeviceConfigState 表示设备配置的运行时状态，记录应用配置后产生的状态信息，用于 Unprepare 时清理资源
type DeviceConfigState struct {
	// MpsControlDaemonID 是 MPS（Multi-Process Service）控制守护进程的 ID
	// MPS 允许多个进程共享 GPU，提高利用率
	// json 标签使这个字段可以被序列化到 checkpoint
	MpsControlDaemonID string `json:"mpsControlDaemonID"`

	// containerEdits 包含需要应用到容器的 CDI 修改
	// 例如：环境变量、设备挂载、钩子等
	// 小写字母开头表示这是私有字段，不会被序列化
	containerEdits *cdiapi.ContainerEdits
}

// DeviceState 是设备状态的中央管理器，协调所有设备相关的操作
// 1. 内嵌 sync.Mutex 保护并发访问
// 2. 使用组合模式，包含多个子管理器
// 3. checkpoint 机制确保状态持久化
type DeviceState struct {
	sync.Mutex

	// cdi 是 CDI 处理器，负责生成 CDI 规范文件
	cdi *CDIHandler

	// tsManager 是时间片管理器
	// 只有当 TimeSlicingSettings 特性门控启用时才使用
	tsManager *TimeSlicingManager

	// mpsManager 是 MPS（Multi-Process Service）管理器
	// 只有当 MPSSupport 特性门控启用时才使用
	mpsManager *MpsManager

	// vfioPciManager 是 VFIO PCI 管理器
	// 只有当 PassthroughSupport 特性门控启用时才使用
	vfioPciManager *VfioPciManager

	// allocatable 是可分配设备的映射
	// key 是设备的规范名称（canonical name），value 是 AllocatableDevice 指针
	allocatable AllocatableDevices

	// config 保存驱动配置，在某些操作中需要访问配置信息
	config *Config

	// nvdevlib 是 NVIDIA 设备库的封装
	nvdevlib *deviceLib

	// checkpointManager 管理状态的持久化
	// checkpoint 保存已准备的 claim 信息，用于：
	// 1. 确保 Prepare 的幂等性
	// 2. 崩溃后恢复状态
	// 3. Unprepare 时获取设备信息
	checkpointManager checkpointmanager.CheckpointManager
}

// NewDeviceState 创建并初始化 DeviceState
//
// 参数：
//   - ctx: 上下文，用于取消操作
//   - config: 驱动配置
//
// 返回：
//   - *DeviceState: 初始化完成的状态管理器
//   - error: 初始化失败时返回错误
//
// 1. 创建设备库（封装 NVML）
// 2. 枚举所有可用设备（GPU、MIG、VFIO）
// 3. 创建 CDI 处理器
// 4. 根据特性门控创建各种管理器
// 5. 生成标准 CDI 规范文件
// 6. 初始化或恢复 checkpoint
func NewDeviceState(ctx context.Context, config *Config) (*DeviceState, error) {
	// ========================================
	// 步骤 1: 创建设备库
	// ========================================
	// containerDriverRoot 是容器内看到的驱动根目录，例如：/run/nvidia/driver
	// 这个路径用于访问 NVIDIA 驱动文件
	containerDriverRoot := root(config.flags.containerDriverRoot)

	// newDeviceLib 初始化 NVML 库并创建封装
	// 如果初始化失败，通常意味着：
	// 1. NVIDIA 驱动没有正确安装
	// 2. 没有 NVIDIA GPU
	// 3. 权限不足
	nvdevlib, err := newDeviceLib(containerDriverRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to create device library: %w", err)
	}

	// ========================================
	// 步骤 2: 枚举所有可能的设备
	// ========================================
	// enumerateAllPossibleDevices 会发现节点上所有的：
	// - 完整 GPU
	// - MIG 设备（如果有）
	// - 可用于 VFIO 的设备（如果启用 passthrough）
	allocatable, err := nvdevlib.enumerateAllPossibleDevices(config)
	if err != nil {
		return nil, fmt.Errorf("error enumerating all possible devices: %w", err)
	}

	// ========================================
	// 步骤 3: 获取设备根目录
	// ========================================
	// devRoot 是设备文件的根目录，如 /dev
	// 不同的容器化环境可能有不同的路径
	devRoot := containerDriverRoot.getDevRoot()
	klog.Infof("using devRoot=%v", devRoot)

	// ========================================
	// 步骤 4: 创建 CDI 处理器
	// ========================================
	// CDI (Container Device Interface) 是一个标准，定义如何将设备及其依赖暴露给容器
	//
	// - WithNvml: 提供 NVML 库实例
	// - WithDeviceLib: 提供设备库
	// - WithDriverRoot: 容器内的驱动根目录
	// - WithDevRoot: 设备文件根目录
	// - WithTargetDriverRoot: 主机上的驱动根目录
	// - WithNVIDIACDIHookPath: CDI 钩子程序路径
	// - WithCDIRoot: CDI 规范文件存放目录
	// - WithVendor: CDI 设备的 vendor 前缀
	hostDriverRoot := config.flags.hostDriverRoot
	cdi, err := NewCDIHandler(
		WithNvml(nvdevlib.nvmllib),
		WithDeviceLib(nvdevlib),
		WithDriverRoot(string(containerDriverRoot)),
		WithDevRoot(devRoot),
		WithTargetDriverRoot(hostDriverRoot),
		WithNVIDIACDIHookPath(config.flags.nvidiaCDIHookPath),
		WithCDIRoot(config.flags.cdiRoot),
		WithVendor(cdiVendor),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %w", err)
	}

	// ========================================
	// 步骤 5: 根据特性门控创建管理器
	// ========================================
	// 使用特性门控可以：
	// 1. 渐进式发布新功能
	// 2. 快速回滚有问题的功能
	// 3. 为不同用户提供不同功能集

	// 时间片管理器 - 用于 GPU 时间共享
	var tsManager *TimeSlicingManager
	if featuregates.Enabled(featuregates.TimeSlicingSettings) {
		tsManager = NewTimeSlicingManager(nvdevlib)
	}

	// MPS 管理器 - 用于多进程服务
	var mpsManager *MpsManager
	if featuregates.Enabled(featuregates.MPSSupport) {
		mpsManager = NewMpsManager(config, nvdevlib, hostDriverRoot, MpsControlDaemonTemplatePath)
	}

	// VFIO PCI 管理器 - 用于 GPU 直通
	var vfioPciManager *VfioPciManager
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		vfioPciManager = NewVfioPciManager(string(containerDriverRoot), string(hostDriverRoot), nvdevlib, true /* nvidiaEnabled */)
	}

	// Validate passthrough support if feature gate is enabled.
	// 验证 passthrough 支持的系统要求
	// 例如：IOMMU 是否启用、vfio 模块是否加载等
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		if err := vfioPciManager.ValidatePassthroughSupport(); err != nil {
			// 使用 klog.Fatalf 而不是返回错误，因为这是一个配置问题，不是临时性错误
			klog.Fatalf("Failed to validate passthrough support: %v", err)
		}
	}

	// ========================================
	// 步骤 6: 生成标准 CDI 规范文件
	// ========================================
	// 这个文件包含所有设备的基础 CDI 规范，位于类似 /etc/cdi/nvidia-gpu.yaml 的位置
	// 容器运行时会读取这个文件了解可用设备
	if err := cdi.CreateStandardDeviceSpecFile(allocatable); err != nil {
		return nil, fmt.Errorf("unable to create base CDI spec file: %v", err)
	}

	// ========================================
	// 步骤 7: 创建 checkpoint 管理器
	// ========================================
	// 在插件的工作目录路径 /var/lib/kubelet/plugins/gpu.nvidia.com/ 初始化 CheckpointManager
	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %v", err)
	}

	// ========================================
	// 步骤 8: 创建 DeviceState 实例
	// ========================================
	state := &DeviceState{
		cdi:               cdi,
		tsManager:         tsManager,
		mpsManager:        mpsManager,
		vfioPciManager:    vfioPciManager,
		config:            config,
		nvdevlib:          nvdevlib,
		checkpointManager: checkpointManager,
	}
	state.allocatable = allocatable

	// ========================================
	// 步骤 9: 检查现有 checkpoint 或创建新的
	// ========================================
	// 列出所有现有的 checkpoint 文件
	checkpoints, err := state.checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("unable to list checkpoints: %v", err)
	}

	// 如果已存在 checkpoint 文件，直接返回
	// 这意味着之前有状态需要恢复
	for _, c := range checkpoints {
		if c == DriverPluginCheckpointFileBasename {
			return state, nil
		}
	}

	// 不存在 checkpoint，创建一个空的
	// 这是首次启动的情况
	if err := state.createCheckpoint(&Checkpoint{}); err != nil {
		return nil, fmt.Errorf("unable to create checkpoint: %v", err)
	}

	return state, nil
}

// Prepare 准备 ResourceClaim 中的设备
//
// 参数：
//   - ctx: 上下文
//   - claim: 需要准备的 ResourceClaim
//
// 返回：
//   - []kubeletplugin.Device: 准备好的设备列表
//   - error: 准备失败时返回错误
//
// 1. 获取锁保护并发访问
// 2. 检查 checkpoint 实现幂等性
// 3. 更新 checkpoint 为 "PrepareStarted"
// 4. 执行设备准备（应用配置、启动服务）
// 5. 生成 CDI 规范文件
// 6. 更新 checkpoint 为 "PrepareCompleted"
func (s *DeviceState) Prepare(ctx context.Context, claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error) {
	s.Lock()
	defer s.Unlock()

	// 获取 claim 的唯一标识符
	claimUID := string(claim.UID)

	// ========================================
	// 步骤 1: 获取当前 checkpoint
	// ========================================
	checkpoint, err := s.getCheckpoint()
	if err != nil {
		return nil, fmt.Errorf("unable to get checkpoint: %v", err)
	}

	// Check for existing 'completed' claim preparation before updating the
	// checkpoint with 'PrepareStarted'. Otherwise, we effectively mark a
	// perfectly prepared claim as only partially prepared, which may have
	// negative side effects during Unprepare() (currently a noop in this case:
	// unprepare noop: claim preparation started but not completed).
	//
	// ========================================
	// 步骤 2: 检查是否已完成准备（幂等性检查）
	// ========================================
	// 如果 claim 已经准备完成（checkpoint 中有记录），直接返回之前的结果，这确保 Prepare 可以被安全地多次调用，避免重复准备导致的问题
	preparedClaim, exists := checkpoint.V2.PreparedClaims[claimUID]
	if exists && preparedClaim.CheckpointState == ClaimCheckpointStatePrepareCompleted {
		// Make this a noop. Associated device(s) has/ave been prepared by us.
		// Prepare() must be idempotent, as it may be invoked more than once per
		// claim (and actual device preparation must happen at most once).
		klog.V(6).Infof("skip prepare: claim %v found in checkpoint", claimUID)
		return preparedClaim.PreparedDevices.GetDevices(), nil
	}

	// In certain scenarios, the same device can be prepared/allocated more than once for different claims
	// due to races between data processing in different goroutines in the scheduler, or when pods are
	// force-deleted while the kubelet still considers the devices allocated.
	// To prevent this, we check whether any device requested in the incoming claim has already been prepared
	// and fail the request if so (unless the prior preparation was performed with admin access).
	// More details: https://github.com/kubernetes/kubernetes/pull/136269
	if err := s.validateNoOverlappingPreparedDevices(checkpoint, claim); err != nil {
		return nil, fmt.Errorf("unable to prepare claim %v: %w", claimUID, err)
	}

	// ========================================
	// 步骤 3: 记录 "PrepareStarted" 状态
	// ========================================
	// 在开始准备前先记录状态，如果准备过程中崩溃，可以知道这个 claim 处于部分准备状态
	err = s.updateCheckpoint(func(checkpoint *Checkpoint) {
		checkpoint.V2.PreparedClaims[claimUID] = PreparedClaim{
			CheckpointState: ClaimCheckpointStatePrepareStarted,
			Status:          claim.Status,
		}
	})
	if err != nil {
		return nil, fmt.Errorf("unable to update checkpoint: %w", err)
	}
	klog.V(6).Infof("checkpoint updated for claim %v", claimUID)

	// ========================================
	// 步骤 4: 执行设备准备
	// ========================================
	// prepareDevices 是核心准备逻辑：
	// 1. 解析配置
	// 2. 验证设备可用性
	// 3. 应用配置（时间片、MPS、VFIO）
	// 4. 返回准备好的设备列表
	// 此处通过 applyConfig() 实现 VFIO 绑定，在 Unprepare 时通过 unprepareVfioDevices() 解绑
	preparedDevices, err := s.prepareDevices(ctx, claim)
	if err != nil {
		return nil, fmt.Errorf("prepare devices failed: %w", err)
	}

	// ========================================
	// 步骤 5: 处理 Passthrough 模式的兄弟设备
	// ========================================
	// 在 Passthrough 模式下，当一个设备被绑定到 vfio-pci：
	// 1. 该设备不再可用作 GPU
	// 2. 同一物理设备的其他表示（如 MIG 切片）也不可用
	// 3. 需要从可分配列表中移除这些"兄弟"设备
	// 此处通过 RemoveSiblingDevices() 移除兄弟设备，在 Unprepare 时通过 discoverSiblingAllocatables() 恢复
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		for _, device := range preparedDevices.GetDevices() {
			allocatableDevice, ok := s.allocatable[device.DeviceName]
			if !ok {
				klog.Warningf("allocatable not found for device: %v", device.DeviceName)
				continue
			}
			s.allocatable.RemoveSiblingDevices(allocatableDevice)
		}
	}

	// ========================================
	// 步骤 6: 创建 claim 特定的 CDI 规范文件
	// ========================================
	// 每个 claim 都有自己的 CDI 规范文件，包含该 claim 特有的设备配置（如环境变量、钩子）
	if err := s.cdi.CreateClaimSpecFile(claimUID, preparedDevices); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %w", err)
	}

	// ========================================
	// 步骤 7: 记录 "PrepareCompleted" 状态
	// ========================================
	// 准备完成后更新 checkpoint，包含准备好的设备信息，用于：
	// 1. 幂等性检查
	// 2. Unprepare 时获取设备信息
	// 3. 崩溃恢复
	err = s.updateCheckpoint(func(checkpoint *Checkpoint) {
		checkpoint.V2.PreparedClaims[claimUID] = PreparedClaim{
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			Status:          claim.Status,
			PreparedDevices: preparedDevices,
		}
	})
	if err != nil {
		return nil, fmt.Errorf("unable to update checkpoint: %w", err)
	}
	klog.V(6).Infof("checkpoint updated for claim %v", claimUID)

	return preparedDevices.GetDevices(), nil
}

// Unprepare 清理 ResourceClaim 的设备
//
// 参数：
//   - ctx: 上下文
//   - claimUID: 需要清理的 claim 的 UID
//
// 返回：
//   - error: 清理失败时返回错误
//
// 1. 获取锁保护并发访问
// 2. 从 checkpoint 获取已准备的设备信息
// 3. 执行设备清理（停止服务、恢复配置）
// 4. 恢复兄弟设备（如果是 Passthrough 模式）
// 5. 删除 CDI 规范文件
// 6. 从 checkpoint 删除 claim 记录
func (s *DeviceState) Unprepare(ctx context.Context, claimUID string) error {
	s.Lock()
	defer s.Unlock()

	// ========================================
	// 步骤 1: 获取 checkpoint
	// ========================================
	checkpoint, err := s.getCheckpoint()
	if err != nil {
		return fmt.Errorf("unable to get checkpoint: %v", err)
	}

	// ========================================
	// 步骤 2: 检查 claim 是否存在
	// ========================================
	pc, exists := checkpoint.V2.PreparedClaims[claimUID]
	if !exists {
		// Not an error: if this claim UID is not in the checkpoint then this
		// device was never prepared or has already been unprepared (assume that
		// Prepare+Checkpoint are done transactionally). Note that
		// claimRef.String() contains namespace, name, UID.
		// 如果 claim 不在 checkpoint 中，可能是从未准备过，或已经被清理，静默返回成功，确保 Unprepare 可以被安全地多次调用
		klog.Infof("unprepare noop: claim not found in checkpoint data: %v", claimUID)
		return nil
	}

	// ========================================
	// 步骤 3: 根据状态决定操作
	// ========================================
	switch pc.CheckpointState {
	case ClaimCheckpointStatePrepareStarted:
		// 准备开始但未完成，说明之前崩溃，这种情况下没有什么需要清理的
		klog.Infof("unprepare noop: claim preparation started but not completed: %v", claimUID)
		return nil
	case ClaimCheckpointStatePrepareCompleted:
		// 准备已完成，需要执行清理
		if err := s.unprepareDevices(ctx, claimUID, pc.PreparedDevices); err != nil {
			return fmt.Errorf("unprepare devices failed: %w", err)
		}
	default:
		return fmt.Errorf("unsupported ClaimCheckpointState: %v", pc.CheckpointState)
	}

	// ========================================
	// 步骤 4: 恢复兄弟设备（Passthrough 模式）
	// ========================================
	// 当 vfio-pci 设备被释放时，需要重新发现兄弟设备，使它们再次可分配
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		for _, device := range pc.PreparedDevices.GetDevices() {
			allocatableDevice, ok := s.allocatable[device.DeviceName]
			if !ok {
				klog.Warningf("allocatable not found for device: %v", device.DeviceName)
				continue
			}
			err := s.discoverSiblingAllocatables(allocatableDevice)
			if err != nil {
				return fmt.Errorf("error discovering sibling allocatables: %w", err)
			}
		}
	}

	// ========================================
	// 步骤 5: 删除 claim 的 CDI 规范文件
	// ========================================
	if err := s.cdi.DeleteClaimSpecFile(claimUID); err != nil {
		return fmt.Errorf("unable to delete CDI spec file for claim: %w", err)
	}

	// ========================================
	// 步骤 6: 从 checkpoint 删除 claim 记录
	// ========================================
	// Unprepare succeeded; reflect that in the node-local checkpoint data.
	delete(checkpoint.V2.PreparedClaims, claimUID)
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFileBasename, checkpoint); err != nil {
		return fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return nil
}

// createCheckpoint 创建新的 checkpoint 文件，参数 cp 是要保存的 checkpoint 数据
func (s *DeviceState) createCheckpoint(cp *Checkpoint) error {
	return s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFileBasename, cp)
}

// getCheckpoint 读取现有的 checkpoint 文件，返回最新版本的 checkpoint 数据
func (s *DeviceState) getCheckpoint() (*Checkpoint, error) {
	checkpoint := &Checkpoint{}
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFileBasename, checkpoint); err != nil {
		return nil, err
	}
	// ToLatestVersion 确保 checkpoint 数据是最新格式，这支持从旧版本升级
	return checkpoint.ToLatestVersion(), nil
}

// updateCheckpoint 更新 checkpoint 文件，参数 f 是一个修改函数，接收当前 checkpoint 并修改它
func (s *DeviceState) updateCheckpoint(f func(*Checkpoint)) error {
	// 先读取当前 checkpoint
	checkpoint, err := s.getCheckpoint()
	if err != nil {
		return fmt.Errorf("unable to get checkpoint: %w", err)
	}

	// 调用修改函数，由 sync.Mutex 保护内存操作的原子性
	f(checkpoint)

	// 写回 checkpoint，由 CreateCheckpoint 内部的文件锁和文件系统调用保证文件操作的原子性
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFileBasename, checkpoint); err != nil {
		return fmt.Errorf("unable to create checkpoint: %w", err)
	}

	return nil
}

// prepareDevices 是设备准备的核心实现
// 1. 解析和验证设备配置
// 2. 将配置映射到对应的设备
// 3. 应用配置（时间片、MPS、VFIO）
// 4. 构建准备好的设备列表
//
// 配置优先级规则：
// - 来自 ResourceClaim 的配置 > 来自 DeviceClass 的配置
// - 列表中后面的配置 > 列表中前面的配置
// - 指定了 Requests 的配置 > 默认配置（Requests 为空）
//
// 参数：
//   - ctx: 上下文，用于取消操作
//   - claim: 包含分配信息的 ResourceClaim
//
// 返回：
//   - PreparedDevices: 准备好的设备列表，包含 CDI 设备 ID
//   - error: 准备失败时返回错误
func (s *DeviceState) prepareDevices(ctx context.Context, claim *resourceapi.ResourceClaim) (PreparedDevices, error) {
	// ========================================
	// 步骤 1: 验证 claim 已被分配
	// ========================================
	// 只有当 DRA 调度器为 claim 分配具体设备后，claim.Status.Allocation 才会被填充
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim not yet allocated")
	}

	// ========================================
	// 步骤 2: 获取设备配置
	// ========================================
	// Retrieve the full set of device configs for the driver.
	// GetOpaqueDeviceConfigs 会：
	// 1. 收集来自 DeviceClass 和 ResourceClaim 的配置
	// 2. 过滤出属于本驱动的配置（通过 DriverName 判断）
	// 3. 解码不透明参数
	// 4. 按优先级排序（低到高）
	configs, err := GetOpaqueDeviceConfigs(
		configapi.StrictDecoder, // 严格解码器，不允许未知字段
		DriverName,              // 只获取本驱动的配置
		claim.Status.Allocation.Devices.Config,
	)
	if err != nil {
		return nil, fmt.Errorf("error getting opaque device configs: %v", err)
	}

	// ========================================
	// 步骤 3: 添加默认配置
	// ========================================
	// Add the default GPU and MIG device Configs to the front of the config
	// list with the lowest precedence. This guarantees there will be at least
	// one of each config in the list with len(Requests) == 0 for the lookup below.
	//
	// 因为用户可能只请求设备而不指定任何配置，默认配置确保每个设备都有一个可用的配置
	// 因为配置列表是按优先级从低到高排列的，最前面的优先级最低，会被后面的配置覆盖，因此默认配置插入到最前面
	configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{
		Requests: []string{}, // 空 Requests 表示适用于所有未明确指定的请求
		Config:   configapi.DefaultGpuConfig(),
	})
	configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{
		Requests: []string{},
		Config:   configapi.DefaultMigDeviceConfig(),
	})
	// VFIO 默认配置只在特性门控启用时添加
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{
			Requests: []string{},
			Config:   configapi.DefaultVfioDeviceConfig(),
		})
	}

	// ========================================
	// 步骤 4: 将配置映射到设备
	// ========================================
	// Look through the configs and figure out which one will be applied to
	// each device allocation result based on their order of precedence and type.
	//
	// configResultsMap 的 key 是配置对象，value 是该配置适用的设备列表
	// 使用 runtime.Object 作为 key 允许存储不同类型的配置
	configResultsMap := make(map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult)

	// 遍历所有分配结果
	for _, result := range claim.Status.Allocation.Devices.Results {
		// 一个 claim 可能包含来自多个驱动的设备，跳过不属于本驱动的设备
		if result.Driver != DriverName {
			continue
		}

		// 验证设备存在于可分配列表中
		device, exists := s.allocatable[result.Device]
		if !exists {
			return nil, fmt.Errorf("requested device is not allocatable: %v", result.Device)
		}
		// only proceed with config mapping if device is healthy.
		// 健康检查：如果设备不健康，拒绝准备
		// 这是一个运行时检查，因为设备可能在分配后变得不健康
		if featuregates.Enabled(featuregates.NVMLDeviceHealthCheck) {
			if !device.IsHealthy() {
				return nil, fmt.Errorf("requested device is not healthy: %v", result.Device)
			}
		}
		// 后面的优先级高，从后向前遍历配置列表
		// slices.Backward 返回一个从后向前的迭代器，这确保高优先级的配置先被匹配
		for _, c := range slices.Backward(configs) {
			// ----------------------------------------
			// 情况 1: 配置明确指定 Requests
			// ----------------------------------------
			// 如果当前请求在配置的 Requests 列表中，使用该配置
			if slices.Contains(c.Requests, result.Request) {
				// 验证配置类型与设备类型匹配
				// 这是类型安全检查：GPU 配置只能应用于 GPU 设备
				if _, ok := c.Config.(*configapi.GpuConfig); ok && device.Type() != GpuDeviceType {
					return nil, fmt.Errorf("cannot apply GPU config to request: %v", result.Request)
				}
				// MIG 配置只能应用于 MIG 设备
				if _, ok := c.Config.(*configapi.MigDeviceConfig); ok && device.Type() != MigDeviceType {
					return nil, fmt.Errorf("cannot apply MIG device config to request: %v", result.Request)
				}
				// VFIO 配置只能应用于 VFIO 设备
				if _, ok := c.Config.(*configapi.VfioDeviceConfig); ok && device.Type() != VfioDeviceType {
					return nil, fmt.Errorf("cannot apply VFIO device config to request: %v", result.Request)
				}
				// 找到匹配的配置，将设备添加到该配置的结果列表
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break // 找到匹配后停止搜索
			}
			// ----------------------------------------
			// 情况 2: 默认配置（Requests 为空）
			// ----------------------------------------
			// 如果配置没有指定 Requests，它是一个"默认"配置，适用于所有没有明确配置的设备
			if len(c.Requests) == 0 {
				// 跳过类型不匹配的默认配置
				// 例如：GPU 设备跳过 MIG 默认配置
				if _, ok := c.Config.(*configapi.GpuConfig); ok && device.Type() != GpuDeviceType {
					continue
				}
				if _, ok := c.Config.(*configapi.MigDeviceConfig); ok && device.Type() != MigDeviceType {
					continue
				}
				if _, ok := c.Config.(*configapi.VfioDeviceConfig); ok && device.Type() != VfioDeviceType {
					continue
				}
				// 使用默认配置
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break
			}
		}
	}

	// ========================================
	// 步骤 5: 验证并应用配置
	// ========================================
	// Normalize, validate, and apply all configs associated with devices that
	// need to be prepared. Track device group configs generated from applying the
	// config to the set of device allocation results.
	// 
	// 1. Normalize - 填充默认值
	// 2. Validate - 验证配置合法性
	// 3. Apply - 应用配置到设备
	preparedDeviceGroupConfigState := make(map[runtime.Object]*DeviceConfigState)
	for c, results := range configResultsMap {
		// Cast the opaque config to a configapi.Interface type
		// 将 runtime.Object 转换为具体的配置类型
		// configapi.Interface 是一个统一的接口，提供 Normalize/Validate 方法
		var config configapi.Interface
		switch castConfig := c.(type) {
		case *configapi.GpuConfig:
			config = castConfig
		case *configapi.MigDeviceConfig:
			config = castConfig
		case *configapi.VfioDeviceConfig:
			config = castConfig
		default:
			return nil, fmt.Errorf("runtime object is not a recognized configuration")
		}

		// Normalize the config to set any implied defaults.
		// 规范化：填充配置中未指定的默认值
		// 例如：如果用户只指定 MPS 开启，Normalize 会填充默认的内存限制
		if err := config.Normalize(); err != nil {
			return nil, fmt.Errorf("error normalizing GPU config: %w", err)
		}

		// Validate the config to ensure its integrity.
		// 验证：确保配置参数在合法范围内
		// 例如：时间片大小在允许范围内、MPS 内存限制合理等
		if err := config.Validate(); err != nil {
			return nil, fmt.Errorf("error validating GPU config: %w", err)
		}

		// Apply the config to the list of results associated with it.
		// 应用：执行实际的配置操作
		// 这可能包括设置时间片、启动 MPS 守护进程、配置 VFIO 等
		configState, err := s.applyConfig(ctx, config, claim, results)
		if err != nil {
			return nil, fmt.Errorf("error applying GPU config: %w", err)
		}

		// Capture the prepared device group config in the map.
		// 保存配置状态，用于后续构建返回结果
		preparedDeviceGroupConfigState[c] = configState
	}

	// ========================================
	// 步骤 6: 构建准备好的设备列表
	// ========================================
	// Walk through each config and its associated device allocation results
	// and construct the list of prepared devices to return.
	//
	// 准备好的设备需要包含 CDI 设备 ID，kubelet 会使用这些 ID 告诉容器运行时如何设置设备
	var preparedDevices PreparedDevices
	for c, results := range configResultsMap {
		// 为每个配置创建一个设备组
		preparedDeviceGroup := PreparedDeviceGroup{
			ConfigState: *preparedDeviceGroupConfigState[c],
		}

		for _, result := range results {
			// 收集 CDI 设备 ID，每个设备可能有多个 CDI ID：
			// 1. 标准设备 ID - 基本的设备暴露
			// 2. Claim 特定设备 ID - 包含 claim 特有的配置
			cdiDevices := []string{}

			// 获取标准设备 ID
			// 格式类似：nvidia.com/gpu=gpu-0
			// 这个 ID 对应标准 CDI 规范文件中的设备定义
			if d := s.cdi.GetStandardDevice(s.allocatable[result.Device]); d != "" {
				cdiDevices = append(cdiDevices, d)
			}

			// 获取 claim 特定设备 ID
			// 格式类似：nvidia.com/claim-{claimUID}=gpu-0
			// 这个 ID 关联的 CDI 规范包含 claim 特有的配置
			// 例如 MPS 客户端环境变量
			if d := s.cdi.GetClaimDevice(string(claim.UID), s.allocatable[result.Device], preparedDeviceGroupConfigState[c].containerEdits); d != "" {
				cdiDevices = append(cdiDevices, d)
			}

			// 创建 kubeletplugin.Device 对象
			// 这是返回给 kubelet 的标准格式
			device := &kubeletplugin.Device{
				Requests:     []string{result.Request}, // 该设备满足哪个请求
				PoolName:     result.Pool,              // 设备池名称（通常是节点名）
				DeviceName:   result.Device,            // 设备名称
				CDIDeviceIDs: cdiDevices,               // CDI 设备 ID 列表
			}

			// 根据设备类型创建对应的 PreparedDevice
			// 使用联合类型存储不同类型的设备信息
			var preparedDevice PreparedDevice
			switch s.allocatable[result.Device].Type() {
			case GpuDeviceType:
				preparedDevice.Gpu = &PreparedGpu{
					Info:   s.allocatable[result.Device].Gpu,
					Device: device,
				}
			case MigDeviceType:
				preparedDevice.Mig = &PreparedMigDevice{
					Info:   s.allocatable[result.Device].Mig,
					Device: device,
				}
			case VfioDeviceType:
				preparedDevice.Vfio = &PreparedVfioDevice{
					Info:   s.allocatable[result.Device].Vfio,
					Device: device,
				}
			}

			preparedDeviceGroup.Devices = append(preparedDeviceGroup.Devices, preparedDevice)
		}

		preparedDevices = append(preparedDevices, &preparedDeviceGroup)
	}
	return preparedDevices, nil
}

// unprepareDevices 清理已准备的设备
// 1. 清理 VFIO 配置（如果使用 passthrough 模式）
// 2. 停止 MPS 控制守护进程（如果启用了 MPS）
// 3. 恢复 GPU 时间片到默认设置
//
// 参数：
//   - ctx: 上下文
//   - claimUID: 正在清理的 claim 的 UID
//   - devices: 之前准备好的设备列表（从 checkpoint 恢复）
//
// 返回：
//   - error: 清理失败时返回错误
func (s *DeviceState) unprepareDevices(ctx context.Context, claimUID string, devices PreparedDevices) error {
	// 遍历每个设备组
	// 一个 claim 可能包含多个设备组，每个组有不同的配置
	for _, group := range devices {
		// ----------------------------------------
		// 步骤 1: 清理 VFIO 设备
		// ----------------------------------------
		// Unconfigure the vfio-pci devices.
		// VFIO 设备需要：
		// 1. 解除 vfio-pci 驱动绑定
		// 2. 恢复原始驱动绑定（如 nvidia 驱动）
		if featuregates.Enabled(featuregates.PassthroughSupport) {
			err := s.unprepareVfioDevices(ctx, group.Devices.VfioDevices())
			if err != nil {
				return err
			}
		}

		// ----------------------------------------
		// 步骤 2: 停止 MPS 守护进程
		// ----------------------------------------
		// Stop any MPS control daemons started for each group of prepared devices.
		// MPS（Multi-Process Service）守护进程需要优雅停止：
		// 1. 发送停止信号
		// 2. 等待现有 GPU 任务完成
		// 3. 清理共享内存和管道资源
		if featuregates.Enabled(featuregates.MPSSupport) {
			// NewMpsControlDaemon 使用 claimUID 和设备组信息
			// 构造出与准备时相同的守护进程标识
			mpsControlDaemon := s.mpsManager.NewMpsControlDaemon(claimUID, group)
			if err := mpsControlDaemon.Stop(ctx); err != nil {
				return fmt.Errorf("error stopping MPS control daemon: %w", err)
			}
		}

		// ----------------------------------------
		// 步骤 3: 恢复默认时间片设置
		// ----------------------------------------
		// Go back to default time-slicing for all full GPUs.
		// 时间片设置是 GPU 级别的，需要恢复到默认值
		// 否则下一个使用该 GPU 的容器会继承之前的设置
		if featuregates.Enabled(featuregates.TimeSlicingSettings) {
			// 获取默认的时间片配置
			tsc := configapi.DefaultGpuConfig().Sharing.TimeSlicingConfig
			// 只对 GPU 设备（不是 MIG）设置时间片
			// MIG 设备有固定的资源分配，不支持时间片
			if err := s.tsManager.SetTimeSlice(group.Devices.Gpus(), tsc); err != nil {
				return fmt.Errorf("error setting timeslice for devices: %w", err)
			}
		}

	}
	return nil
}

// getAllocatableVfioDevice 根据 UUID 查找可分配的 VFIO 设备
// 因为在 unprepareVfioDevices 中，只有 PreparedDevice 信息，需要找到对应的 AllocatableDevice 来获取完整的 VFIO 信息
//
// 参数：
//   - uuid: VFIO 设备的 UUID
//
// 返回：
//   - *AllocatableDevice: 找到的设备
//   - error: 如果设备不存在
func (s *DeviceState) getAllocatableVfioDevice(uuid string) (*AllocatableDevice, error) {
	// 遍历所有可分配设备
	for _, allocatable := range s.allocatable {
		// 只检查 VFIO 类型设备
		if allocatable.Type() != VfioDeviceType {
			continue
		}
		// UUID 匹配
		if allocatable.Vfio.UUID == uuid {
			return allocatable, nil
		}
	}
	return nil, fmt.Errorf("allocatable device not found for vfio device: %v", uuid)
}

// unprepareVfioDevices 清理 VFIO 设备配置
// VFIO (Virtual Function I/O) 用于设备直通：
// 1. 将 GPU 从 nvidia 驱动解绑
// 2. 绑定到 vfio-pci 驱动
// 3. 这样虚拟机可以直接访问 GPU
//
// 清理时需要执行反向操作
//
// 参数：
//   - ctx: 上下文
//   - devices: 需要清理的 VFIO 设备列表
//
// 返回：
//   - error: 清理失败时返回错误
func (s *DeviceState) unprepareVfioDevices(ctx context.Context, devices PreparedDeviceList) error {
	for _, device := range devices {
		// 根据 UUID 找到对应的 AllocatableDevice
		vfioAllocatable, err := s.getAllocatableVfioDevice(device.Vfio.Info.UUID)
		if err != nil {
			return fmt.Errorf("error getting allocatable device for vfio device: %w", err)
		}
		// 解除 VFIO 配置，恢复原始驱动绑定
		if err := s.vfioPciManager.Unconfigure(ctx, vfioAllocatable.Vfio); err != nil {
			return fmt.Errorf("error unconfiguring vfio device: %w", err)
		}
	}
	return nil
}

// discoverSiblingAllocatables 重新发现设备的"兄弟"设备
// 同一物理 GPU 可以有多种表示形式：
// - GPU 设备：完整的 GPU
// - MIG 设备：GPU 的切片
// - VFIO 设备：用于直通的设备
//
// 当一个表示被使用时，其他表示变得不可用，当它被释放时，需要重新发现其他表示
// 例如：
// - VFIO 设备被释放后，需要重新发现对应的 GPU 和 MIG 设备
// - GPU 设备被释放后（passthrough 模式），需要重新发现 VFIO 设备
//
// 参数：
//   - device: 刚被释放的设备
//
// 返回：
//   - error: 发现失败时返回错误
func (s *DeviceState) discoverSiblingAllocatables(device *AllocatableDevice) error {
	switch device.Type() {
	case GpuDeviceType:
		// GPU 被释放后，检查是否需要重新发现 VFIO 设备
		// 只有在 vfioEnabled 时才有对应的 VFIO 设备
		if !device.Gpu.vfioEnabled {
			return nil
		}
		// 发现对应的 VFIO 设备并添加到可分配列表
		vfio, err := s.nvdevlib.discoverVfioDevice(device.Gpu)
		if err != nil {
			return fmt.Errorf("error discovering vfio device: %w", err)
		}
		s.allocatable[vfio.CanonicalName()] = vfio
	case VfioDeviceType:
		// VFIO 设备被释放后，重新发现对应的 GPU 和 MIG 设备
		// 使用 PCIe Bus ID 来定位物理设备
		gpu, migs, err := s.nvdevlib.discoverGPUByPCIBusID(device.Vfio.pcieBusID)
		if err != nil {
			return fmt.Errorf("error discovering gpu by pci bus id: %w", err)
		}
		// 添加 GPU 到可分配列表
		s.allocatable[gpu.CanonicalName()] = gpu
		// 更新 VFIO 设备的父引用
		device.Vfio.parent = gpu.Gpu
		// 添加所有 MIG 设备到可分配列表
		for _, mig := range migs {
			s.allocatable[mig.CanonicalName()] = mig
		}
	case MigDeviceType:
		// TODO: Implement once dynamic MIG is supported.
		// 动态 MIG 尚不支持，暂时跳过
		return nil
	}
	return nil
}

// applyConfig 应用设备配置，根据配置类型调用不同的应用方法：
// - GpuConfig/MigDeviceConfig -> applySharingConfig (处理时间片和 MPS)
// - VfioDeviceConfig -> applyVfioDeviceConfig (处理 VFIO 绑定)
//
// 参数：
//   - ctx: 上下文
//   - config: 已验证的配置
//   - claim: ResourceClaim
//   - results: 该配置适用的设备列表
//
// 返回：
//   - *DeviceConfigState: 配置应用后的状态
//   - error: 应用失败时返回错误
func (s *DeviceState) applyConfig(ctx context.Context, config configapi.Interface, claim *resourceapi.ResourceClaim, results []*resourceapi.DeviceRequestAllocationResult) (*DeviceConfigState, error) {
	switch castConfig := config.(type) {
	case *configapi.GpuConfig:
		// GPU 配置使用共享配置处理器
		return s.applySharingConfig(ctx, castConfig.Sharing, claim, results)
	case *configapi.MigDeviceConfig:
		// MIG 设备配置也使用共享配置处理器
		// 因为 MIG 设备同样支持时间片和 MPS
		return s.applySharingConfig(ctx, castConfig.Sharing, claim, results)
	case *configapi.VfioDeviceConfig:
		// VFIO 配置使用专门的处理器
		return s.applyVfioDeviceConfig(ctx, castConfig, claim, results)
	default:
		return nil, fmt.Errorf("unknown config type: %T", castConfig)
	}
}

// applySharingConfig 应用 GPU 共享配置
//
// GPU 共享有两种主要模式：
//
// 1. 时间片（Time-Slicing）：
//   - 多个进程轮流使用 GPU
//   - 实现简单但有上下文切换开销
//   - 适合批处理工作负载
//   - 通过 NVML API 设置
//
// 2. MPS（Multi-Process Service）：
//   - 多个进程同时共享 GPU 计算资源
//   - 需要运行 MPS 守护进程
//   - 更低的延迟，适合推理工作负载
//   - 需要配置内存限制防止 OOM
//   - 通过启动 MPS control daemon 实现
//
// 参数：
//   - ctx: 上下文
//   - config: 共享配置（包含时间片或 MPS 设置）
//   - claim: ResourceClaim（用于生成唯一 ID）
//   - results: 适用的设备列表
//
// 返回：
//   - *DeviceConfigState: 包含 MPS 守护进程 ID 和容器编辑
//   - error: 应用失败时返回错误
func (s *DeviceState) applySharingConfig(ctx context.Context, config configapi.Sharing, claim *resourceapi.ResourceClaim, results []*resourceapi.DeviceRequestAllocationResult) (*DeviceConfigState, error) {
	// Get the list of claim requests this config is being applied over.
	// 收集此配置适用的所有请求名称，用于错误消息，帮助调试
	var requests []string
	for _, r := range results {
		requests = append(requests, r.Request)
	}

	// Get the list of allocatable devices this config is being applied over.
	// 收集此配置适用的所有设备，时间片和 MPS 需要知道具体的设备信息
	allocatableDevices := make(AllocatableDevices)
	for _, r := range results {
		allocatableDevices[r.Device] = s.allocatable[r.Device]
	}

	// Declare a device group state object to populate.
	// 创建配置状态对象，用于记录应用结果
	var configState DeviceConfigState

	// ----------------------------------------
	// 时间片设置
	// ----------------------------------------
	// Apply time-slicing settings (if available and feature gate enabled).
	// 时间片允许多个进程共享 GPU，通过轮流执行实现
	// 设置包括时间片大小（duration）等参数
	if featuregates.Enabled(featuregates.TimeSlicingSettings) && config.IsTimeSlicing() {
		tsc, err := config.GetTimeSlicingConfig()
		if err != nil {
			return nil, fmt.Errorf("error getting timeslice config for requests '%v' in claim '%v': %w", requests, claim.UID, err)
		}
		if tsc != nil {
			// 通过 NVML API 设置 GPU 的时间片参数
			err = s.tsManager.SetTimeSlice(allocatableDevices, tsc)
			if err != nil {
				return nil, fmt.Errorf("error setting timeslice config for requests '%v' in claim '%v': %w", requests, claim.UID, err)
			}
		}
	}

	// ----------------------------------------
	// MPS 设置
	// ----------------------------------------
	// Apply MPS settings (if available and feature gate enabled).
	// MPS 需要启动一个控制守护进程来管理 GPU 共享
	// 守护进程负责协调多个客户端对 GPU 的访问
	if featuregates.Enabled(featuregates.MPSSupport) && config.IsMps() {
		// 获取 MPS 配置（内存限制、线程限制等）
		mpsc, err := config.GetMpsConfig()
		if err != nil {
			return nil, fmt.Errorf("error getting MPS configuration: %w", err)
		}
		// 为这个 claim 创建 MPS 控制守护进程
		// 使用 claim UID 作为标识，确保唯一性
		mpsControlDaemon := s.mpsManager.NewMpsControlDaemon(string(claim.UID), allocatableDevices)
		// 启动守护进程
		if err := mpsControlDaemon.Start(ctx, mpsc); err != nil {
			return nil, fmt.Errorf("error starting MPS control daemon: %w", err)
		}
		// 等待守护进程就绪
		// MPS 守护进程需要一些时间来初始化
		if err := mpsControlDaemon.AssertReady(ctx); err != nil {
			return nil, fmt.Errorf("MPS control daemon is not yet ready: %w", err)
		}
		// 记录守护进程 ID，用于后续清理
		configState.MpsControlDaemonID = mpsControlDaemon.GetID()
		// 获取容器需要的环境变量等 CDI 编辑
		// 例如 CUDA_MPS_PIPE_DIRECTORY 环境变量
		configState.containerEdits = mpsControlDaemon.GetCDIContainerEdits()
	}

	return &configState, nil
}

// applyVfioDeviceConfig 应用 VFIO 设备配置
//
// VFIO (Virtual Function I/O) 用于设备直通，主要用于虚拟化场景：
// 1. 将 GPU 从原始驱动（nvidia）解绑
// 2. 绑定到 vfio-pci 驱动
// 3. 虚拟机可以直接访问物理 GPU
//
// 这种模式下，GPU 被独占使用，提供最佳性能，但一个 GPU 只能被一个虚拟机使用
//
// 参数：
//   - ctx: 上下文
//   - config: VFIO 配置
//   - claim: ResourceClaim
//   - results: 适用的设备列表
//
// 返回：
//   - *DeviceConfigState: 配置状态（VFIO 目前不需要额外状态）
//   - error: 配置失败时返回错误
func (s *DeviceState) applyVfioDeviceConfig(ctx context.Context, config *configapi.VfioDeviceConfig, claim *resourceapi.ResourceClaim, results []*resourceapi.DeviceRequestAllocationResult) (*DeviceConfigState, error) {
	// 检查特性门控
	if !featuregates.Enabled(featuregates.PassthroughSupport) {
		return nil, nil
	}
	var configState DeviceConfigState

	// Configure the vfio-pci devices.
	// 配置每个 VFIO 设备
	for _, r := range results {
		info := s.allocatable[r.Device]
		// Configure 会：
		// 1. 将设备从当前驱动解绑
		// 2. 绑定到 vfio-pci 驱动
		// 3. 设置必要的权限
		err := s.vfioPciManager.Configure(ctx, info.Vfio)
		if err != nil {
			return nil, err
		}
	}

	return &configState, nil
}

// GetOpaqueDeviceConfigs 从分配配置中提取并解码设备配置，用于处理 DRA 的配置系统。
// 在 DRA 中，配置来自两个地方，优先级从低到高：
//
// 1. DeviceClass（设备类）- 由管理员定义的默认配置
//   - 适用于整个集群或一类设备
//   - 优先级较低，会被 claim 配置覆盖
//
// 2. ResourceClaim（资源声明）- 由用户指定的特定配置
//   - 优先级较高
//   - 允许用户覆盖 DeviceClass 的默认设置
//
// 参数：
//   - decoder: 用于解码配置的 runtime.Decoder
//   - driverName: 当前驱动的名称，用于过滤配置
//   - possibleConfigs: 所有可能的配置列表
//
// 返回：
//   - []*OpaqueDeviceConfig: 按优先级排序的配置列表（低到高）
//   - error: 解码失败时返回错误
func GetOpaqueDeviceConfigs(
	decoder runtime.Decoder,
	driverName string,
	possibleConfigs []resourceapi.DeviceAllocationConfiguration,
) ([]*OpaqueDeviceConfig, error) {
	// ========================================
	// 步骤 1: 按来源分类配置
	// ========================================
	// Collect all configs in order of reverse precedence.
	// 分别收集来自 Class 和 Claim 的配置
	var classConfigs []resourceapi.DeviceAllocationConfiguration
	var claimConfigs []resourceapi.DeviceAllocationConfiguration
	var candidateConfigs []resourceapi.DeviceAllocationConfiguration
	for _, config := range possibleConfigs {
		switch config.Source {
		case resourceapi.AllocationConfigSourceClass:
			// 来自 DeviceClass 的配置
			classConfigs = append(classConfigs, config)
		case resourceapi.AllocationConfigSourceClaim:
			// 来自 ResourceClaim 的配置
			claimConfigs = append(claimConfigs, config)
		default:
			return nil, fmt.Errorf("invalid config source: %v", config.Source)
		}
	}
	// 按优先级顺序合并：Class 配置在前（优先级低），Claim 配置在后（优先级高）
	candidateConfigs = append(candidateConfigs, classConfigs...)
	candidateConfigs = append(candidateConfigs, claimConfigs...)

	// ========================================
	// 步骤 2: 解码配置
	// ========================================
	// Decode all configs that are relevant for the driver.
	var resultConfigs []*OpaqueDeviceConfig
	for _, config := range candidateConfigs {
		// If this is nil, the driver doesn't support some future API extension
		// and needs to be updated.
		// 当前只支持不透明参数
		// 如果 Opaque 为 nil，说明使用了新的 API 扩展
		if config.Opaque == nil {
			return nil, fmt.Errorf("only opaque parameters are supported by this driver")
		}

		// Configs for different drivers may have been specified because a
		// single request can be satisfied by different drivers. This is not
		// an error -- drivers must skip over other driver's configs in order
		// to support this.
		// 跳过不属于本驱动的配置
		// 这是多驱动支持的关键：一个请求可能被多个驱动满足
		if config.Opaque.Driver != driverName {
			continue
		}

		// 解码不透明参数
		// 使用提供的解码器将原始 JSON/YAML 转换为 Go 对象
		decodedConfig, err := runtime.Decode(decoder, config.Opaque.Parameters.Raw)
		if err != nil {
			return nil, fmt.Errorf("error decoding config parameters: %w", err)
		}

		// 创建 OpaqueDeviceConfig 对象
		resultConfig := &OpaqueDeviceConfig{
			Requests: config.Requests, // 此配置适用的请求列表
			Config:   decodedConfig,   // 解码后的配置对象
		}

		resultConfigs = append(resultConfigs, resultConfig)
	}

	return resultConfigs, nil
}

// UpdateDeviceHealthStatus 更新设备的健康状态
//
// 这个函数被健康监控器调用，用于更新设备的健康状态。
// 当 NVML 检测到设备异常（如 XID 错误）时，会调用此函数
// 将设备标记为不健康。
//
// 设备健康状态的影响：
// 1. 不健康的设备会从 ResourceSlice 中移除，不再被调度
// 2. 已分配给该设备的 claim 在准备时会失败
// 3. 需要管理员干预来恢复设备
//
// 线程安全：此函数使用锁保护，可以从任何 goroutine 调用
//
// 参数：
//   - d: 要更新的设备
//   - hs: 新的健康状态（Healthy 或 Unhealthy）
func (s *DeviceState) UpdateDeviceHealthStatus(d *AllocatableDevice, hs HealthStatus) {
	// 获取锁保护状态更新
	s.Lock()
	defer s.Unlock()

	// 根据设备类型更新对应的健康状态字段
	switch d.Type() {
	case GpuDeviceType:
		d.Gpu.Health = hs
	case MigDeviceType:
		d.Mig.Health = hs
	default:
		// VFIO 设备目前不支持健康状态
		klog.V(6).Infof("Cannot update health status for unknown device type: %s", d.Type())
		return
	}
	klog.V(4).Infof("Updated device: %s health status to %s", d.UUID(), hs)
}

// requestedNonAdminDevices returns the set of device names requested by the claim,
// excluding admin-access allocations.
func (s *DeviceState) requestedNonAdminDevices(claim *resourceapi.ResourceClaim) map[string]struct{} {
	requested := make(map[string]struct{}, len(claim.Status.Allocation.Devices.Results))

	for _, r := range claim.Status.Allocation.Devices.Results {
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

// validateNoOverlappingPreparedDevices checks whether the given claim requests any device that is
// already allocated (non-admin) to a different claim that has completed preparation.
func (s *DeviceState) validateNoOverlappingPreparedDevices(checkpoint *Checkpoint, claim *resourceapi.ResourceClaim) error {
	claimUID := string(claim.UID)

	// Get the set of requested non-admin devices for the current claim.
	requestedDevices := s.requestedNonAdminDevices(claim)
	if len(requestedDevices) == 0 {
		return nil
	}

	for existingClaimUID, pc := range checkpoint.V2.PreparedClaims {
		// Skip the current claim.
		if existingClaimUID == claimUID {
			continue
		}
		if pc.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}

		// Get the non-admin devices from the prepared claim in the checkpoint.
		// We allow overlapping device allocations only if they are requested with admin access.
		preparedDevices := pc.GetNonAdminDevices()
		if len(preparedDevices) == 0 {
			continue
		}

		// Check for overlaps between requested devices from the current claim and others.
		for device := range requestedDevices {
			if _, found := preparedDevices[device]; found {
				return fmt.Errorf(
					"requested device %s is already allocated to different claim %s",
					device, existingClaimUID,
				)
			}
		}
	}
	return nil
}

// TODO: Dynamic MIG is not yet supported with structured parameters.
// Refactor this to allow for the allocation of statically partitioned MIG
// devices.
//
// func (s *DeviceState) prepareMigDevices(claimUID string, allocated *nascrd.AllocatedMigDevices) (*PreparedMigDevices, error) {
// 	prepared := &PreparedMigDevices{}
//
// 	for _, device := range allocated.Devices {
// 		if _, exists := s.allocatable[device.ParentUUID]; !exists {
// 			return nil, fmt.Errorf("allocated GPU does not exist: %v", device.ParentUUID)
// 		}
//
// 		parent := s.allocatable[device.ParentUUID]
//
// 		if !parent.migEnabled {
// 			return nil, fmt.Errorf("cannot prepare a GPU with MIG mode disabled: %v", device.ParentUUID)
// 		}
//
// 		if _, exists := parent.migProfiles[device.Profile]; !exists {
// 			return nil, fmt.Errorf("MIG profile %v does not exist on GPU: %v", device.Profile, device.ParentUUID)
// 		}
//
// 		placement := nvml.GpuInstancePlacement{
// 			Start: uint32(device.Placement.Start),
// 			Size:  uint32(device.Placement.Size),
// 		}
//
// 		migInfo, err := s.nvdevlib.createMigDevice(parent.GpuInfo, parent.migProfiles[device.Profile].profile, &placement)
// 		if err != nil {
// 			return nil, fmt.Errorf("error creating MIG device: %w", err)
// 		}
//
// 		prepared.Devices = append(prepared.Devices, migInfo)
// 	}
//
// 	return prepared, nil
// }
//
// func (s *DeviceState) unprepareMigDevices(claimUID string, devices *PreparedDevices) error {
// 	for _, device := range devices.Mig.Devices {
// 		err := s.nvdevlib.deleteMigDevice(device)
// 		if err != nil {
// 			return fmt.Errorf("error deleting MIG device for %v: %w", device.uuid, err)
// 		}
// 	}
// 	return nil
//}
