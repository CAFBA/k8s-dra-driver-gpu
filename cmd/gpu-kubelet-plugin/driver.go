/*
 * Copyright (c) 2022-2024, NVIDIA CORPORATION.  All rights reserved.
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
// driver.go - DRA Kubelet Plugin 驱动核心实现
// ============================================================================
// 这个文件是 NVIDIA GPU DRA (Dynamic Resource Allocation) 驱动的核心实现。
// 它实现了 Kubernetes DRA kubelet plugin 接口，负责：
// 1. 向 Kubernetes 发布节点上的 GPU 资源 (ResourceSlice)
// 2. 处理 ResourceClaim 的 Prepare/Unprepare 请求
// 3. 监控 GPU 设备健康状态
// ============================================================================

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	// resourceapi 是 Kubernetes DRA 的核心 API 类型包
	// 包含 ResourceClaim、ResourceSlice 等重要类型定义
	// 这些类型是 DRA 驱动与 Kubernetes 交互的基础
	resourceapi "k8s.io/api/resource/v1"

	// types.UID 是 Kubernetes 对象的唯一标识符类型
	// 每个 Kubernetes 对象都有一个全局唯一的 UID，用于精确标识对象
	"k8s.io/apimachinery/pkg/types"

	// runtime 包提供统一的错误处理机制
	// HandleErrorWithContext 是 Kubernetes 推荐的错误处理方式
	"k8s.io/apimachinery/pkg/util/runtime"

	// coreclientset 是 Kubernetes 核心 API 的客户端接口
	// 用于与 API Server 进行通信，执行 CRUD 操作
	coreclientset "k8s.io/client-go/kubernetes"

	// kubeletplugin 是 DRA kubelet 插件的官方框架库
	// 提供了 Helper（用于发布资源）、PrepareResult（准备结果）等类型，以及 Start 函数用于启动插件
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	// resourceslice 包提供构建和管理 ResourceSlice 的工具
	// ResourceSlice 是 DRA 中描述节点可用资源的核心对象
	"k8s.io/dynamic-resource-allocation/resourceslice"

	"k8s.io/klog/v2"

	// featuregates 是 NVIDIA 驱动的特性开关包
	// 允许通过配置启用或禁用特定功能，如健康检查、MPS 支持等
	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/featuregates"

	// flock 是文件锁包，用于跨进程同步
	// 文件锁比进程内的互斥锁更持久，即使进程崩溃重启也能保持一致性
	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/flock"
)

// DriverPrepUprepFlockPath is the path to a lock file used to make sure
// that calls to nodePrepareResource() / nodeUnprepareResource() never
// interleave, node-globally.
//
// DriverPrepUprepFlockFileName 定义 Prepare/Unprepare 操作使用的锁文件名
// 1. nodePrepareResource 和 nodeUnprepareResource 可能被 kubelet 并发调用
// 2. 这两个操作会修改共享状态：设备配置、CDI 文件、checkpoint 等
// 3. 文件锁是跨进程的，可以防止多个驱动实例同时操作，文件锁是持久的，进程崩溃后锁会自动释放
// 4. "pu" 是 Prepare/Unprepare 的缩写，表明这个锁的专用性
const DriverPrepUprepFlockFileName = "pu.lock"

// deviceHealthMonitor 定义设备健康监控器的接口，而不是直接使用具体实现
// 1. 解耦：driver 不需要知道健康监控的具体实现（可以是 NVML、sysfs 等）
// 2. 可测试：在单元测试中可以注入 mock 实现
// 3. 可扩展：未来可以轻松添加新的健康检查方式
type deviceHealthMonitor interface {
	// Start 启动健康监控
	// ctx 用于控制监控器的生命周期，当 ctx 取消时监控器应该优雅退出
	Start(context.Context) error

	// Stop 停止健康监控
	// 应该关闭 Unhealthy channel，释放所有资源
	Stop()

	// Unhealthy 返回一个只读 channel
	// 当检测到不健康的设备时，通过这个 channel 发送
	// 1. 异步通知：健康检查是后台持续运行的
	// 2. 非阻塞：生产者不会因为消费者慢而阻塞
	// 3. 解耦：监控逻辑和处理逻辑完全分离
	Unhealthy() <-chan *AllocatableDevice
}

// driver 是 DRA kubelet plugin 的核心结构体
// 实现 kubeletplugin.DRAPlugin 接口，负责处理所有 DRA 相关操作
//
// DRAPlugin 接口要求实现的方法：
// - PrepareResourceClaims: 准备资源声明中的设备
// - UnprepareResourceClaims: 清理资源声明中的设备
// - HandleError: 处理内部错误
type driver struct {
	// client 是 Kubernetes 核心 API 客户端
	//
	// kubeletplugin.Start() 函数通过 kubeletplugin.KubeClient() 选项接收客户端
	// KubeClient() 的签名是：func KubeClient(kubeClient kubernetes.Interface) Option
	// kubernetes.Interface 就是 coreclientset.Interface 的别名，是一个接口类型
	// 此处的 client 在 kubeletplugin.Start() 内部用于创建两个客户端:
	// 	kubeClient:     o.kubeClient,    原始客户端
	// 	resourceClient: draclient.New(o.kubeClient),  用于 DRA 资源操作
	//
	// - func New(clientSet kubernetes.Interface) *Client
	// - *Client 实现 cgoresource.ResourceV1Interface 接口
	// 为什么 draclient.New() 返回的不是 cgoresource.ResourceV1Interface？
	// - 因为 *Client 是具体类型，它不仅实现接口，还有额外的方法
	// - 但是可以把它赋值给 ResourceV1Interface 类型的字段，因为它实现这个接口
	//
	// 必须通过 draclient 创建 resourceClient：
	// - 在运行时自动探测 K8s 集群支持的 DRA API 版本（v1alpha2, v1beta1, v1）
	// - 当调用 Create/Get 时，它会先试最新 API，失败则自动降级到旧 API
	// - 它会在内存中自动转换对象（v1 ↔ v1beta1 ↔ v1alpha2）
	// - 这样同一份驱动代码就能运行在不同版本的 K8s 集群上
	
	// 接口 vs 具体类型（关于 Mock）：
	//    - 什么是"具体类型"（Concrete Type）：
	//      - 具体类型是实际的数据结构，包含完整的实现
	//      - 例如：clientset 实现 kubernetes.Interface，它包含所有 API 组的客户端实现
	//      - 具体类型的行为是固定的，无法在运行时改变
	//
	//    - 什么是"接口类型"（Interface Type）：
	//      - 接口定义了一组方法签名，不包含实现
	//      - 例如：kubernetes.Interface 定义 CoreV1()、ResourceV1() 等方法
	//      - 任何实现这些方法的类型都可以赋值给接口变量
	//
	//    - 什么是 Mock：
	//      - Mock 是测试中使用的模拟对象，实现了与真实对象相同的接口
	//      - Mock 对象的行为可以由测试代码控制（例如返回预设的值）
	//      - 使用 Mock 可以隔离被测代码，避免依赖外部服务（如 Kubernetes API Server）
	//
	//    - 为什么接口类型便于 Mock：
	//      - 如果使用具体类型 clientset，测试时必须连接真实的 K8s API Server
	//      - 如果使用接口类型 kubernetes.Interface，可以注入 fake clientset
	//      - fake clientset 是 Kubernetes 提供的测试工具，实现了 kubernetes.Interface
	//      - 测试代码可以控制 fake clientset 返回的数据，验证各种场景
	//
	//    - 示例对比：
	//      使用具体类型（难以测试）
	//      var client *clientset.Clientset  // 必须连接真实 API Server
	//
	//      使用接口类型（易于测试）
	//      var client kubernetes.Interface  // 可以是真实 clientset 或 fake clientset
	//
	//      测试时注入 fake
	//      fakeClient := fake.NewSimpleClientset()
	//      driver := &driver{client: fakeClient}  // 可以！
	client coreclientset.Interface

	// pluginhelper 是 kubeletplugin 库提供的辅助对象
	// 1. 向 kubelet 注册插件（通过 unix socket）
	// 2. 发布 ResourceSlice 到 API Server
	// 3. 处理 gRPC 通信的底层细节
	// 4. 管理插件的生命周期（启动、停止）
	//
	// 1. Helper 是有状态对象，使用指针类型：
	//    - 维护 gRPC 服务器实例（用于与 kubelet 通信）
	//    - 缓存已发布的 ResourceSlice 状态
	//    - 管理与 kubelet 的连接状态
	//    - 跟踪插件注册状态和生命周期
	//
	// 2. 避免值拷贝导致的问题：
	//    - 如果使用值类型，每次传递都会复制整个对象
	//    - 这会创建多个独立的状态副本，状态更新只会影响副本，不会影响原始对象
	//    - 多个 goroutine 可能操作不同的副本，导致状态不一致
	//
	// 3. 确保共享状态：
	//    - 使用指针确保所有代码都操作同一个 Helper 实例
	//    - 所有对 PublishResources() 的调用都更新同一份缓存
	//    - Stop() 可以正确关闭共享的 gRPC 服务器
	//
	// 4. 方法接收者要求：
	//    - Helper 的方法都使用指针接收者 (*Helper)
	//    - 如果存储为值类型，调用方法时会自动取地址
	//    - 但这个地址是临时的，不是原始 Helper 的地址
	//    - 会导致方法调用在临时副本上执行，状态丢失
	//
	// 5. 性能考虑：
	//    - Helper 可能包含大量数据（设备列表、缓存、连接等）
	//    - 值拷贝会带来不必要的内存和 CPU 开销
	//    - 指针只占 8 字节（64位系统），拷贝成本极低
	pluginhelper *kubeletplugin.Helper

	// state 管理所有设备相关的状态
	// 1. allocatable: 节点上可分配的设备列表
	// 2. checkpoint: 已准备的 claim 状态（用于崩溃恢复）
	// 3. cdi: CDI 文件管理器
	// 4. 各种设备管理器：tsManager、mpsManager、vfioPciManager
	state *DeviceState

	// pulock 是 Prepare/Unprepare 操作的文件锁
	pulock *flock.Flock

	// healthcheck 是一个简单的 HTTP 健康检查服务
	// Kubernetes 使用 liveness/readiness 探针检查容器健康，这个服务提供 HTTP 端点供探针调用
	healthcheck *healthcheck

	// deviceHealthMonitor 监控 GPU 硬件健康状态
	// 当检测到 XID 错误、ECC 错误等问题时，会通知 driver 更新 ResourceSlice
	// 使用接口类型使得可以替换不同的健康检查实现
	deviceHealthMonitor deviceHealthMonitor

	// wg 用于等待所有后台 goroutine 完成
	wg sync.WaitGroup
}

// NewDriver 创建并初始化一个新的 DRA 驱动实例
// 参数：
//   - ctx: 上下文，用于取消初始化过程和控制生命周期
//   - config: 配置对象，包含命令行参数、客户端集等
//
// 返回：
//   - *driver: 初始化完成的驱动实例
//   - error: 初始化失败时返回错误
// 
// 初始化流程：
// 1. 创建 DeviceState - 枚举 GPU、初始化各种管理器
// 2. 启动 kubelet plugin - 注册到 kubelet
// 3. 启动健康检查服务 - 供 Kubernetes 探针使用
// 4. 启动设备健康监控 - 监控 GPU 硬件状态
// 5. 发布资源 - 让调度器知道有哪些 GPU 可用
func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	// ========================================
	// 步骤 1: 创建设备状态管理器
	// ========================================
	// NewDeviceState 是一个复杂的初始化过程，包括：
	// 1. 初始化 NVML 库（NVIDIA 管理库）
	// 2. 枚举节点上所有的 GPU、MIG 设备、VFIO 设备
	// 3. 创建 CDI Handler 用于生成容器设备规范
	// 4. 初始化时间片管理器、MPS 管理器、VFIO 管理器
	// 5. 恢复或创建 checkpoint（用于崩溃恢复）
	// 这一步失败通常意味着 GPU 驱动没有正确安装或 NVML 无法初始化
	state, err := NewDeviceState(ctx, config)
	if err != nil {
		return nil, err
	}

	// ========================================
	// 步骤 2: 创建 Prepare/Unprepare 锁文件路径
	// ========================================
	// 锁文件放在驱动插件数据目录下
	// DriverPluginPath() 返回类似 /var/lib/kubelet/plugins/gpu.nvidia.com/
	// 这个目录由 kubelet 确保存在且有正确权限
	puLockPath := filepath.Join(config.DriverPluginPath(), DriverPrepUprepFlockFileName)

	// ========================================
	// 步骤 3: 创建 driver 实例
	// ========================================
	driver := &driver{
		client: config.clientsets.Core,     // Kubernetes 核心 API 客户端
		state:  state,                      // 设备状态管理器
		pulock: flock.NewFlock(puLockPath), // 创建文件锁对象（不获取锁）
	}

	// ========================================
	// 步骤 4: 启动 kubelet plugin
	// ========================================
	// kubeletplugin.Start 是 DRA 框架的核心函数，创建 gRPC 服务器并向 kubelet 注册插件
	//
	// 参数详解：
	// - driver: 实现 DRAPlugin 接口的对象，处理 Prepare/Unprepare 请求
	// - KubeClient: Kubernetes 客户端，用于创建/更新 ResourceSlice
	// - NodeName: 当前节点名称，ResourceSlice 会关联到这个节点
	// - DriverName: 驱动名称（如 gpu.nvidia.com），用于匹配 ResourceClass
	// - Serialize(false): 不序列化请求，允许并发处理多个 claim
	// - RegistrarDirectoryPath: kubelet 插件注册目录，通常是 /var/lib/kubelet/plugins_registry/
	// - PluginDataDirectoryPath: 插件数据目录，存放 socket 文件、checkpoint 等
	helper, err := kubeletplugin.Start(
		ctx,
		driver,
		kubeletplugin.KubeClient(driver.client),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(DriverName),
		kubeletplugin.Serialize(false),
		kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
	)
	if err != nil {
		return nil, err
	}
	driver.pluginhelper = helper

	// ========================================
	// 步骤 5: 启动 HTTP 健康检查服务
	// ========================================
	// 健康检查是 Kubernetes 容器生命周期管理的重要部分
	// kubelet 会定期调用健康检查端点：
	// - liveness probe: 检查进程是否存活，失败会重启容器
	// - readiness probe: 检查是否就绪，失败会从 Service 端点移除
	healthcheck, err := startHealthcheck(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}
	driver.healthcheck = healthcheck

	// ========================================
	// 步骤 6: 启动 NVML 设备健康监控（条件启用）
	// ========================================
	// 只有当 NVMLDeviceHealthCheck 特性门控启用时才运行
	// 特性门控允许渐进式发布新功能，出问题时可以快速回滚
	if featuregates.Enabled(featuregates.NVMLDeviceHealthCheck) {
		// 创建基于 NVML 的健康监控器
		// 它会监听 XID 错误、检查 ECC 状态等
		deviceHealthMonitor, err := newNvmlDeviceHealthMonitor(config, state.allocatable, state.nvdevlib)
		if err != nil {
			return nil, fmt.Errorf("failed to create NVML device health monitor: %w", err)
		}

		// 启动监控器，开始后台检查
		if err := deviceHealthMonitor.Start(ctx); err != nil {
			return nil, fmt.Errorf("failed to start device health monitor: %w", err)
		}
		driver.deviceHealthMonitor = deviceHealthMonitor

		// 启动后台 goroutine 处理健康事件
		// wg.Add(1) 确保 Shutdown 时会等待这个 goroutine 完成
		driver.wg.Add(1)
		go func() {
			// defer 确保即使发生 panic，wg.Done() 也会被调用
			defer driver.wg.Done()
			// 处理健康事件，直到 ctx 取消或 channel 关闭
			driver.deviceHealthEvents(ctx, config.flags.nodeName)
		}()
	}

	// ========================================
	// 步骤 7: 发布资源到 Kubernetes
	// ========================================
	// 这是 DRA 驱动的核心职责
	// publishResources 会创建/更新 ResourceSlice 对象
	// 调度器通过 ResourceSlice 知道每个节点有哪些设备可用
	if err := driver.publishResources(ctx, config); err != nil {
		return nil, err
	}

	return driver, nil
}

// Shutdown 优雅地关闭驱动，释放所有资源
//
// 关闭顺序遵循"先停止接收，再等待完成，最后清理资源"的原则：
// 1. 停止健康检查服务 - 不再接收新的探针请求
// 2. 停止设备健康监控 - 不再产生新的健康事件
// 3. 等待后台 goroutine 完成 - 确保进行中的操作能正常结束
// 4. 停止 kubelet plugin - 断开与 kubelet 的连接
//
// 返回 error 是为了接口一致性，目前总是返回 nil
func (d *driver) Shutdown() error {
	// 防御性编程：处理 driver 为 nil 的情况
	// 这可能发生在 NewDriver 失败后调用 Shutdown 的场景
	if d == nil {
		return nil
	}

	// 步骤 1: 停止健康检查服务
	// 这会关闭 HTTP 服务器，之后的探针请求会失败
	// 但这是正常的，因为容器正在关闭
	if d.healthcheck != nil {
		d.healthcheck.Stop()
	}

	// 步骤 2: 停止设备健康监控
	// 这会关闭 Unhealthy channel，使 deviceHealthEvents goroutine 退出
	if d.deviceHealthMonitor != nil {
		d.deviceHealthMonitor.Stop()
	}

	// 步骤 3: 等待所有后台 goroutine 完成
	// 这是优雅关闭的关键，确保没有 goroutine 被强制终止
	// 如果有 goroutine 没有正确响应退出信号，这里会阻塞
	d.wg.Wait()

	// 步骤 4: 停止 kubelet plugin
	// 这会：
	// 1. 关闭 gRPC 服务器
	// 2. 从 kubelet 注销插件
	// 3. 删除 socket 文件
	d.pluginhelper.Stop()
	return nil
}

// PrepareResourceClaims 准备一批资源声明
// 当 Pod 被调度到这个节点后，kubelet 在启动容器前会调用这个方法
// 准备的设备会通过 CDI 传递给容器运行时
// 参数：
//   - ctx: 上下文，包含超时和取消信息
//   - claims: 需要准备的 ResourceClaim 列表
//
// 返回：
//   - map[types.UID]kubeletplugin.PrepareResult: 每个 claim 的准备结果
//
// 1. 外层错误（框架级）- PrepareResourceClaims 返回的 error
// 2. 内层错误（Claim 级）- nodePrepareResource 返回的 PrepareResult 的 Err 字段
// 这样设计允许部分成功：某些 claim 准备成功，某些失败
func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	klog.V(6).Infof("PrepareResourceClaims called with %d claim(s)", len(claims))

	// 使用 claim.UID 作为 key 确保唯一性，UID 是 Kubernetes 对象的全局唯一标识符
	results := make(map[types.UID]kubeletplugin.PrepareResult)

	// 因为 nodePrepareResource 内部有文件锁，此处选择串行逐个处理每个 claim
	for _, claim := range claims {
		results[claim.UID] = d.nodePrepareResource(ctx, claim)
	}

	return results, nil
}

// UnprepareResourceClaims 清理一批资源声明，在下述情况中调用
// 1. Pod 正常终止后
// 2. Pod 被驱逐时
// 3. ResourceClaim 被删除时
// 参数：
//   - ctx: 上下文
//   - claimRefs: 需要清理的 claim 引用列表
//
// 返回：
//   - map[types.UID]error: 每个 claim 的清理结果
//   - error: 框架级错误
//
// claimRefs 只包含引用信息（namespace、name、UID）
// 不包含完整的 claim 对象，因为此时 claim 可能已被删除
// 这就是为什么 Unprepare 依赖 checkpoint 而不是 claim 对象
func (d *driver) UnprepareResourceClaims(ctx context.Context, claimRefs []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.V(6).Infof("UnprepareResourceClaims called with %d claim(s)", len(claimRefs))

	results := make(map[types.UID]error)

	for _, claimRef := range claimRefs {
		results[claimRef.UID] = d.nodeUnprepareResource(ctx, claimRef)
	}

	return results, nil
}

// HandleError 处理 DRA 插件内部错误
// 参数：
//   - ctx: 上下文，包含请求相关信息
//   - err: 发生的错误
//   - msg: 错误描述信息
//
// 这个方法被 DRA 框架调用来处理后台操作中的错误，例如 ResourceSlice 发布失败等情况
func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	// For now we just follow the advice documented in the DRAPlugin API docs.
	// See: https://pkg.go.dev/k8s.io/apimachinery/pkg/util/runtime#HandleErrorWithContext
	//
	// 使用 Kubernetes 标准的错误处理机制
	// HandleErrorWithContext 会：
	// 1. 记录错误日志（包含堆栈信息）
	// 2. 更新错误相关的 metrics
	// 3. 如果配置错误处理钩子，会调用它们
	runtime.HandleErrorWithContext(ctx, err, msg)
}

// nodePrepareResource 准备单个 ResourceClaim 中的设备
// 1. 获取互斥锁（防止并发修改）
// 2. 调用 state.Prepare 执行实际准备工作
// 3. 如果是 Passthrough 模式，重新发布 ResourceSlice
// 4. 返回准备好的设备信息
//
// 参数：
//   - ctx: 上下文
//   - claim: 需要准备的 ResourceClaim 对象
//
// 返回：
//   - kubeletplugin.PrepareResult: 包含准备好的设备列表或错误
func (d *driver) nodePrepareResource(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	// ========================================
	// 步骤 1: 获取 Prepare/Unprepare 互斥锁
	// ========================================
	// 1. Prepare 和 Unprepare 可能被并发调用（不同的 Pod）
	// 2. 它们会修改共享资源：checkpoint、CDI 文件、设备配置
	// 3. 没有锁会导致数据不一致或设备状态错乱
	//
	// 超时设置为 10 秒的原因：
	// - 太短：正常操作可能因为争抢锁而超时
	// - 太长：死锁问题难以发现
	// - 10 秒足够完成大多数操作，同时能及时发现问题
	release, err := d.pulock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error acquiring prep/unprep lock: %w", err),
		}
	}
	// defer 确保锁一定会被释放
	// 即使后续代码 panic，锁也会在函数返回前释放
	defer release()

	// ========================================
	// 步骤 2: 执行设备准备
	// ========================================
	// 1. 检查 checkpoint，防止重复准备（幂等性）
	// 2. 更新 checkpoint 为 "PrepareStarted"
	// 3. 解析设备配置（时间片、MPS、VFIO 等）
	// 4. 应用配置到设备
	// 5. 生成 CDI 规范文件
	// 6. 更新 checkpoint 为 "PrepareCompleted"
	devs, err := d.state.Prepare(ctx, claim)

	if err != nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claim %v: %w", claim.UID, err),
		}
	}

	// ========================================
	// 步骤 3: 更新 ResourceSlice（Passthrough 模式）
	// ========================================
	// Passthrough 是 GPU 直通给虚拟机的模式（vfio-pci）
	// 当设备被绑定到 vfio-pci 驱动时：
	// 1. 一个 PCI 设备在同一时间只能由一个驱动管理，当 GPU 绑定到 vfio-pci 后，nvidia 驱动失去对它的控制权
	// 2. nvidia 驱动无法访问已经被 vfio-pci 占用的设备，该设备不再作为普通 GPU 可用
	// 3. 相关的"兄弟"设备（如同一 GPU 的 MIG 切片）也不可用
	// 4. 必须从 ResourceSlice 中移除这些设备
	// 5. 否则调度器可能会把不存在的设备分配给其他 Pod
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		// Re-advertise updated resourceslice after preparing devices.
		// 重新发布 ResourceSlice，移除已被使用的设备
		if err = d.publishResources(ctx, d.state.config); err != nil {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("error preparing devices for claim %v: %w", claim.UID, err),
			}
		}
	}

	// 记录成功信息并返回结果
	// devs 包含准备好的设备信息，包括 CDI 设备 ID
	// kubelet 会将这些 CDI ID 传递给容器运行时
	klog.Infof("Returning newly prepared devices for claim '%v': %v", claim.UID, devs)
	return kubeletplugin.PrepareResult{Devices: devs}
}

// nodeUnprepareResource 清理单个 ResourceClaim 的设备
// 1. 获取互斥锁
// 2. 调用 state.Unprepare 执行清理
// 3. 如果是 Passthrough 模式，重新发布 ResourceSlice
// 参数：
//   - ctx: 上下文
//   - claimNs: claim 的命名空间引用（包含 namespace、name、UID）
//
// 返回：
//   - error: 清理错误，nil 表示成功
func (d *driver) nodeUnprepareResource(ctx context.Context, claimNs kubeletplugin.NamespacedObject) error {
	// 步骤 1: 获取 Prepare/Unprepare 互斥锁
	release, err := d.pulock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		return fmt.Errorf("error acquiring prep/unprep lock: %w", err)
	}
	defer release()

	// 步骤 2: 执行设备清理
	// 1. 从 checkpoint 获取已准备的设备信息
	// 2. 停止 MPS daemon（如果有）
	// 3. 恢复时间片设置为默认值
	// 4. 删除 CDI 规范文件
	// 5. 更新 checkpoint，删除该 claim 的记录
	if err := d.state.Unprepare(ctx, string(claimNs.UID)); err != nil {
		return fmt.Errorf("error unpreparing devices for claim %v: %w", claimNs.UID, err)
	}

	// 步骤 3: 更新 ResourceSlice（Passthrough 模式）
	// 设备被释放后，需要重新添加到可用设备列表
	// 这样调度器才能将这些设备分配给新的 Pod
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		// Re-advertise updated resourceslice after unpreparing devices.
		if err = d.publishResources(ctx, d.state.config); err != nil {
			return fmt.Errorf("error publishing resources: %w", err)
		}
	}

	return nil
}

// publishResources 将节点上的可用设备发布为 ResourceSlice
// 1. 每个节点可以有多个 ResourceSlice，但 NVIDIA Driver 的设计中每次只放一个元素
// 2. 描述该节点上可用的设备及其属性（如显存、计算能力）
// 3. 调度器使用 ResourceSlice 信息做调度决策
// 4. 当设备状态变化时（分配、释放、故障）需要更新 ResourceSlice
//
// 参数：
//   - ctx: 上下文
//   - config: 配置对象
//
// 返回：
//   - error: 发布失败时返回错误
func (d *driver) publishResources(ctx context.Context, config *Config) error {
	// Enumerate the set of GPU, MIG and VFIO devices and publish them
	// 枚举当前节点上所有可用的 GPU 设备，构建 resourceslice.Slice 对象
	var resourceSlice resourceslice.Slice
	for _, device := range d.state.allocatable {
		// GetDevice() 返回 resourceslice.Device 类型，包含设备名称、类型、属性等信息
		resourceSlice.Devices = append(resourceSlice.Devices, device.GetDevice())
	}

	// 构建 DriverResources 对象，NVIDIA Driver 的设计中每个节点一个 Pool ，包含该节点的所有 ResourceSlice
	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			// 使用节点名作为 Pool 名称，这样 ResourceSlice 会与特定节点关联
			// 切片只是接口要求，NVIDIA Driver 的设计中每次只放一个 ResourceSlice
			config.flags.nodeName: {Slices: []resourceslice.Slice{resourceSlice}},
		},
	}

	// 通过 pluginhelper 发布资源到 API Server
	// 这会创建或更新 ResourceSlice 对象
	if err := d.pluginhelper.PublishResources(ctx, resources); err != nil {
		return err
	}

	return nil
}

// deviceHealthEvents 处理设备健康状态变化事件，在后台 goroutine 中运行，监听健康监控器发送的不健康设备通知
// 参数：
//   - ctx: 上下文，用于接收取消信号
//   - nodeName: 当前节点名称，用于发布 ResourceSlice
//
// 1. 从 deviceHealthMonitor.Unhealthy() channel 接收不健康设备
// 2. 标记设备为不健康状态
// 3. 重新发布 ResourceSlice，排除不健康的设备
// 4. 调度器不会再将工作负载调度到这些设备上
func (d *driver) deviceHealthEvents(ctx context.Context, nodeName string) {
	klog.V(4).Info("Starting to watch for device health notifications")
	// 无限循环，直到 ctx 取消或 channel 关闭
	for {
		select {
		case <-ctx.Done():
			// 上下文被取消（驱动正在关闭）
			klog.V(6).Info("Stop processing device health notifications")
			return
		case device, ok := <-d.deviceHealthMonitor.Unhealthy():
			// 从 channel 接收不健康设备通知
			if !ok {
				// NVML based deviceHealthMonitor is expected to close only during driver Shutdown.
				// channel 被关闭，通常是因为驱动正在关闭
				klog.V(6).Info("Health monitor channel closed")
				return
			}
			uuid := device.UUID()

			// 记录警告日志，因为设备不健康是需要关注的事件
			klog.Warningf("Received unhealthy notification for device: %s", uuid)

			// 检查设备是否已经被标记为不健康
			// 避免重复处理同一个设备
			if !device.IsHealthy() {
				klog.V(6).Infof("Device: %s is aleady marked unhealthy. Skip republishing ResourceSlice", uuid)
				continue
			}

			// Mark device as unhealthy.
			// 在内存中标记设备为不健康
			d.state.UpdateDeviceHealthStatus(device, Unhealthy)

			// Republish resource slice with only healthy devices
			// There is no remediation loop right now meaning if the unhealthy device is fixed,
			// driver needs to be restarted to publish the ResourceSlice with all devices
			//
			// 重新构建 ResourceSlice，只包含健康的设备
			// 注意：目前没有恢复机制，如果设备恢复健康，需要重启驱动
			var resourceSlice resourceslice.Slice
			for _, dev := range d.state.allocatable {
				uuid := dev.UUID()
				if dev.IsHealthy() {
					// 健康的设备添加到 ResourceSlice
					klog.V(6).Infof("Device: %s is healthy, added to ResoureSlice", uuid)
					resourceSlice.Devices = append(resourceSlice.Devices, dev.GetDevice())
				} else {
					// 不健康的设备不添加，相当于从可用列表中移除
					klog.Warningf("Device: %s is unhealthy, will be removed from ResoureSlice", uuid)
				}
			}

			klog.V(4).Info("Rebulishing resourceslice with healthy devices")
			resources := resourceslice.DriverResources{
				Pools: map[string]resourceslice.Pool{
					nodeName: {Slices: []resourceslice.Slice{resourceSlice}},
				},
			}

			// NOTE: We only log an error on publish failure and do not retry.
			// If this publish fails, our in-memory health update succeeds but the
			// ResourceSlice in the API server remains stale and still advertises the
			// now-unhealthy device as allocatable. Until a later publish succeeds,
			// the scheduler and other consumers will continue to see the unhealthy
			// device as available, and new pods may be placed onto hardware we know
			// is unusable. If publishes continue to fail (e.g., API server issues),
			// the cluster can remain in this inconsistent state indefinitely.
			// This is a temporary compromise while device taints/tolerations (KEP-5055)
			// are available as a Beta feature. An interim improvement could be adding
			// a retry/backoff or switch to patch updates instead of full republish.
			//
			// 发布更新后的 ResourceSlice
			// 注意：这里只记录错误，不重试
			// 这是一个已知的局限性：如果发布失败，API Server 中的 ResourceSlice
			// 仍然包含不健康的设备，可能导致调度问题
			// 未来可以通过 device taints/tolerations 机制改进
			if err := d.pluginhelper.PublishResources(ctx, resources); err != nil {
				klog.Errorf("Failed to publish resources after device health status update: %v", err)
			} else {
				klog.V(4).Info("Successfully republished resources without unhealthy device")
			}
		}
	}
}

// TODO: implement loop to remove CDI files from the CDI path for claimUIDs
//       that have been removed from the AllocatedClaims map.
// TODO: 实现清理循环，删除已经从 AllocatedClaims 中移除的 claim 对应的 CDI 文件
// 这是一个待实现的功能，用于清理孤儿 CDI 文件
// func (d *driver) cleanupCDIFiles(wg *sync.WaitGroup) chan error {
// 	errors := make(chan error)
// 	return errors
// }
//
// TODO: implement loop to remove mpsControlDaemon folders from the mps
//       path for claimUIDs that have been removed from the AllocatedClaims map.
// TODO: 实现清理循环，删除已经从 AllocatedClaims 中移除的 claim 对应的 MPS 控制守护进程目录
// func (d *driver) cleanupMpsControlDaemonArtifacts(wg *sync.WaitGroup) chan error {
// 	errors := make(chan error)
// 	return errors
// }
