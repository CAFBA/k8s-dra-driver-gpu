
# NVIDIA DRA Driver 完整流程图和代码示例

## 总体架构图

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         Kubernetes 集群                                  │
│                                                                          │
│  ┌──────────────┐      ┌──────────────┐      ┌──────────────┐         │
│  │   Scheduler  │      │  API Server  │      │   Kubelet    │         │
│  │              │      │              │      │              │         │
│  │  - 读取      │◄────►│  - 存储      │◄────►│  - 调用     │         │
│  │    ResourceSlice│    │    ResourceSlice│    │    Plugin   │         │
│  │  - 分配      │      │    ResourceClaim│    │  - 准备设备  │         │
│  │    设备      │      │              │      │              │         │
│  └──────────────┘      └──────────────┘      └──────┬───────┘         │
│                                                      │                  │
└──────────────────────────────────────────────────────┼──────────────────┘
                                                       │
                                                       │ gRPC/Unix Socket
                                                       │
                    ┌──────────────────────────────────▼────────────────┐
                    │     NVIDIA GPU DRA Kubelet Plugin                 │
                    │                                                    │
                    │  ┌────────────┐  ┌──────────────┐                │
                    │  │   Driver   │  │ DeviceState  │                │
                    │  │            │  │              │                │
                    │  │ - Prepare  │──│ - Checkpoint │                │
                    │  │ - Unprepare│  │ - Allocatable│                │
                    │  │ - Publish  │  │ - CDI        │                │
                    │  └────────────┘  └──────────────┘                │
                    │                                                    │
                    │  ┌─────────────────────────────────────────────┐ │
                    │  │         Manager Components                  │ │
                    │  │                                             │ │
                    │  │  ┌────────────┐  ┌────────────┐           │ │
                    │  │  │ CDIHandler │  │ TSManager  │           │ │
                    │  │  └────────────┘  └────────────┘           │ │
                    │  │                                             │ │
                    │  │  ┌────────────┐  ┌────────────┐           │ │
                    │  │  │ MPSManager │  │VfioPCIMgr  │           │ │
                    │  │  └────────────┘  └────────────┘           │ │
                    │  └─────────────────────────────────────────────┘ │
                    └────────────────────────────────────────────────────┘
                                        │
                                        │ NVML API
                                        │
                    ┌────────────────────▼──────────────────────┐
                    │         NVIDIA Driver (内核态)             │
                    └────────────────────┬──────────────────────┘
                                        │
                                        │
                    ┌────────────────────▼──────────────────────┐
                    │          GPU Hardware (物理设备)           │
                    └───────────────────────────────────────────┘
```

---

## 阶段 1: 插件启动与资源发布

### 1.1 插件初始化流程图

```
main()
  │
  ├─► 1. 解析命令行参数和配置
  │     ├─ flags: nodeName, pluginPath, cdiRoot 等
  │     └─ 创建 Kubernetes 客户端
  │
  ├─► 2. NewDriver(ctx, config)
  │     │
  │     ├─► 2.1 NewDeviceState(ctx, config)
  │     │     │
  │     │     ├─► 2.1.1 初始化 NVML 库
  │     │     │     └─ nvdevlib = newDeviceLib(containerDriverRoot)
  │     │     │
  │     │     ├─► 2.1.2 枚举所有 GPU 设备
  │     │     │     └─ allocatable = nvdevlib.enumerateAllPossibleDevices(config)
  │     │     │          │
  │     │     │          ├─► enumerateGpus()      // 完整 GPU
  │     │     │          ├─► enumerateMigDevices() // MIG 设备
  │     │     │          └─► enumerateVfioDevices() // VFIO 设备
  │     │     │
  │     │     ├─► 2.1.3 创建 CDI Handler
  │     │     │     └─ cdi = NewCDIHandler(options...)
  │     │     │
  │     │     ├─► 2.1.4 创建管理器（根据特性门控）
  │     │     │     ├─ tsManager = NewTimeSlicingManager()
  │     │     │     ├─ mpsManager = NewMpsManager()
  │     │     │     └─ vfioPciManager = NewVfioPciManager()
  │     │     │
  │     │     ├─► 2.1.5 生成标准 CDI 规范文件
  │     │     │     └─ cdi.CreateStandardDeviceSpecFile(allocatable)
  │     │     │
  │     │     └─► 2.1.6 初始化/恢复 Checkpoint
  │     │           └─ checkpointManager.CreateCheckpoint()
  │     │
  │     ├─► 2.2 启动 Kubelet Plugin
  │     │     └─ pluginhelper = kubeletplugin.Start(ctx, driver, options...)
  │     │          │
  │     │          ├─► 创建 gRPC 服务器
  │     │          ├─► 注册到 kubelet
  │     │          └─► 启动资源发布循环
  │     │
  │     ├─► 2.3 启动健康检查服务
  │     │     └─ healthcheck = startHealthcheck(ctx, config)
  │     │
  │     ├─► 2.4 启动设备健康监控（可选）
  │     │     └─ deviceHealthMonitor.Start(ctx)
  │     │          └─ go deviceHealthEvents(ctx, nodeName)
  │     │
  │     └─► 2.5 发布资源到 Kubernetes
  │           └─ driver.publishResources(ctx, config)
  │
  └─► 3. 等待信号并优雅关闭
        └─ driver.Shutdown()
```

### 1.2 资源发布详细流程

```go
// 函数调用链：
// main() → NewDriver() → publishResources() → pluginhelper.PublishResources()

func (d *driver) publishResources(ctx context.Context, config *Config) error {
    // 步骤 1: 构建 ResourceSlice
    // allocatable 是从 DeviceState 获取的所有可分配设备
    var resourceSlice resourceslice.Slice
    for _, device := range d.state.allocatable {
        // 每个 AllocatableDevice 转换为 resourceapi.Device
        resourceSlice.Devices = append(resourceSlice.Devices, device.GetDevice())
    }
    
    // 步骤 2: 构建 DriverResources
    // 组织资源到 Pool 中，通常每个节点一个 Pool
    resources := resourceslice.DriverResources{
        Pools: map[string]resourceslice.Pool{
            config.flags.nodeName: {
                Slices: []resourceslice.Slice{resourceSlice},
            },
        },
    }
    
    // 步骤 3: 发布到 API Server
    // pluginhelper 内部会：
    // 1. 创建/更新 ResourceSlice 对象
    // 2. 设置 ownerReference 到节点
    // 3. 处理版本冲突
    if err := d.pluginhelper.PublishResources(ctx, resources); err != nil {
        return err
    }
    
    return nil
}

// 设备转换示例：从 GpuInfo 到 resourceapi.Device
func (d *GpuInfo) GetDevice() resourceapi.Device {
    return resourceapi.Device{
        Name: d.CanonicalName(), // "gpu-0"
        
        // 描述性属性（用于选择器匹配）
        Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
            "type": {StringValue: ptr.To("gpu")},
            "uuid": {StringValue: &d.UUID},
            "productName": {StringValue: &d.productName},
            "architecture": {StringValue: &d.architecture},
            "cudaComputeCapability": {
                VersionValue: ptr.To(semver.MustParse(d.cudaComputeCapability).String()),
            },
            // ... 更多属性
        },
        
        // 可量化容量（用于资源分配）
        Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
            "memory": {
                Value: *resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
            },
        },
    }
}
```

### 1.3 发布后的 ResourceSlice 示例

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: node1-gpu-nvidia-com-abc123
  ownerReferences:
  - apiVersion: v1
    kind: Node
    name: node1
    uid: xxx-xxx-xxx
spec:
  nodeName: node1
  driver: gpu.nvidia.com
  pool:
    name: node1
    resourceSliceCount: 1
    generation: 1
  devices:
  - name: gpu-0
    attributes:
      type:
        string: "gpu"
      uuid:
        string: "GPU-12345678-1234-1234-1234-123456789012"
      productName:
        string: "NVIDIA A100-SXM4-40GB"
      architecture:
        string: "Ampere"
      cudaComputeCapability:
        version: "8.0"
    capacity:
      memory:
        value: "42949672960"  # 40GB
  - name: gpu-1
    # ... 类似结构
```

---

## 阶段 2: 用户申请资源

### 2.1 用户创建 ResourceClaim

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-gpu-claim
  namespace: default
spec:
  devices:
    requests:
    - name: gpu-request
      deviceClassName: nvidia-gpu
      selectors:
      - cel:
          expression: |
            device.attributes["architecture"] == "Ampere" &&
            device.capacity["memory"] >= quantity("32Gi")
      count: 1
    config:
    - opaque:
        driver: gpu.nvidia.com
        parameters:
          apiVersion: gpu.nvidia.com/v1beta1
          kind: GpuConfig
          sharing:
            strategy: TimeSlicing
            timeSlicingConfig:
              interval: Default
```

### 2.2 调度器分配流程图

```
Scheduler
  │
  ├─► 1. 监听新创建的 Pod
  │     └─ Pod 引用了 ResourceClaim "my-gpu-claim"
  │
  ├─► 2. 读取 ResourceSlice
  │     └─ 获取每个节点的可用设备列表
  │
  ├─► 3. 过滤节点
  │     ├─ 检查设备数量是否满足
  │     ├─ 应用 CEL 选择器表达式
  │     └─ 筛选出候选节点
  │
  ├─► 4. 评分和选择最佳节点
  │     └─ 选择 node1
  │
  └─► 5. 更新 ResourceClaim
        └─ 在 status.allocation 中记录分配结果
```

### 2.3 ResourceClaim 分配后状态

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-gpu-claim
  namespace: default
spec:
  # ... (与上面相同)
status:
  allocation:
    devices:
      results:
      - request: gpu-request      # 对应 spec.devices.requests[0].name
        driver: gpu.nvidia.com
        pool: node1                # 分配的节点/Pool
        device: gpu-0              # 分配的具体设备
      config:
      - source: FromClaim          # 配置来源
        opaque:
          driver: gpu.nvidia.com
          parameters:
            apiVersion: gpu.nvidia.com/v1beta1
            kind: GpuConfig
            sharing:
              strategy: TimeSlicing
              timeSlicingConfig:
                interval: Default
  reservedFor:
  - resource: pods
    name: my-pod
    uid: yyy-yyy-yyy
```

---

## 阶段 3: Kubelet 准备设备 (Prepare)

### 3.1 Prepare 完整流程图

```
Kubelet
  │
  ├─► 1. Pod 被调度到本节点
  │     └─ Pod.spec.resourceClaims 引用 "my-gpu-claim"
  │
  ├─► 2. 通过 gRPC 调用插件
  │     └─ PrepareResourceClaims([claim])
  │
  │     ┌─────────────────────────────────────┐
  │     │      Plugin (driver.go)              │
  │     │                                      │
  │     ├─► PrepareResourceClaims()           │
  │     │     │                                │
  │     │     └─► nodePrepareResource()       │
  │     │           │                          │
  │     │           ├─► 1. 获取 pulock        │
  │     │           │     └─ 10秒超时         │
  │     │           │                          │
  │     │           ├─► 2. state.Prepare()    │
  │     │           │                          │
  │     │           └─► 3. 重新发布 ResourceSlice (Passthrough模式)
  │     └─────────────────────────────────────┘
  │
  └─► 3. 获取 CDI 设备 ID
        └─ 传递给容器运行时
```

### 3.2 DeviceState.Prepare() 详细流程

```go
// 函数调用链：
// kubelet → PrepareResourceClaims() → nodePrepareResource() → state.Prepare()

func (s *DeviceState) Prepare(ctx context.Context, claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error) {
    s.Lock()
    defer s.Unlock()
    
    claimUID := string(claim.UID)
    
    // ========================================
    // 步骤 1: 幂等性检查
    // ========================================
    checkpoint, err := s.getCheckpoint()
    if err != nil {
        return nil, err
    }
    
    // 如果已经准备完成，直接返回之前的结果
    preparedClaim, exists := checkpoint.V2.PreparedClaims[claimUID]
    if exists && preparedClaim.CheckpointState == ClaimCheckpointStatePrepareCompleted {
        klog.V(6).Infof("skip prepare: claim %v already prepared", claimUID)
        return preparedClaim.PreparedDevices.GetDevices(), nil
    }
    
    // ========================================
    // 步骤 2: 记录 "PrepareStarted" 状态
    // ========================================
    err = s.updateCheckpoint(func(checkpoint *Checkpoint) {
        checkpoint.V2.PreparedClaims[claimUID] = PreparedClaim{
            CheckpointState: ClaimCheckpointStatePrepareStarted,
            Status:          claim.Status,
        }
    })
    if err != nil {
        return nil, err
    }
    
    // ========================================
    // 步骤 3: 执行设备准备
    // ========================================
    preparedDevices, err := s.prepareDevices(ctx, claim)
    if err != nil {
        return nil, err
    }
    
    // ========================================
    // 步骤 4: 处理 Passthrough 模式（移除兄弟设备）
    // ========================================
    if featuregates.Enabled(featuregates.PassthroughSupport) {
        for _, device := range preparedDevices.GetDevices() {
            allocatableDevice, ok := s.allocatable[device.DeviceName]
            if !ok {
                continue
            }
            s.allocatable.RemoveSiblingDevices(allocatableDevice)
        }
    }
    
    // ========================================
    // 步骤 5: 创建 claim 特定的 CDI 规范文件
    // ========================================
    if err := s.cdi.CreateClaimSpecFile(claimUID, preparedDevices); err != nil {
        return nil, err
    }
    
    // ========================================
    // 步骤 6: 记录 "PrepareCompleted" 状态
    // ========================================
    err = s.updateCheckpoint(func(checkpoint *Checkpoint) {
        checkpoint.V2.PreparedClaims[claimUID] = PreparedClaim{
            CheckpointState: ClaimCheckpointStatePrepareCompleted,
            Status:          claim.Status,
            PreparedDevices: preparedDevices,
        }
    })
    if err != nil {
        return nil, err
    }
    
    return preparedDevices.GetDevices(), nil
}
```

### 3.3 prepareDevices() 核心逻辑

```go
// 函数调用链：
// Prepare() → prepareDevices() → applyConfig()

func (s *DeviceState) prepareDevices(ctx context.Context, claim *resourceapi.ResourceClaim) (PreparedDevices, error) {
    // ========================================
    // 步骤 1: 解析 Opaque 配置
    // ========================================
    configs, err := GetOpaqueDeviceConfigs(
        configapi.StrictDecoder,
        DriverName,
        claim.Status.Allocation.Devices.Config,
    )
    
    // ========================================
    // 步骤 2: 添加默认配置
    // ========================================
    // 确保每种设备类型都有默认配置
    configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{
        Requests: []string{},
        Config:   configapi.DefaultGpuConfig(),
    })
    configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{
        Requests: []string{},
        Config:   configapi.DefaultMigDeviceConfig(),
    })
    
    // ========================================
    // 步骤 3: 配置映射
    // ========================================
    // 为每个设备分配结果找到对应的配置
    configResultsMap := make(map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult)
    for _, result := range claim.Status.Allocation.Devices.Results {
        if result.Driver != DriverName {
            continue
        }
        
        device, exists := s.allocatable[result.Device]
        if !exists {
            return nil, fmt.Errorf("device not found: %v", result.Device)
        }
        
        // 健康检查
        if featuregates.Enabled(featuregates.NVMLDeviceHealthCheck) {
            if !device.IsHealthy() {
                return nil, fmt.Errorf("device unhealthy: %v", result.Device)
            }
        }
        
        // 根据优先级选择配置
        for _, c := range slices.Backward(configs) {
            if slices.Contains(c.Requests, result.Request) || len(c.Requests) == 0 {
                // 类型匹配检查
                if _, ok := c.Config.(*configapi.GpuConfig); ok && device.Type() != GpuDeviceType {
                    continue
                }
                configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
                break
            }
        }
    }
    
    // ========================================
    // 步骤 4: 应用配置
    // ========================================
    preparedDeviceGroupConfigState := make(map[runtime.Object]*DeviceConfigState)
    for c, results := range configResultsMap {
        var config configapi.Interface
        switch castConfig := c.(type) {
        case *configapi.GpuConfig:
            config = castConfig
        case *configapi.MigDeviceConfig:
            config = castConfig
        case *configapi.VfioDeviceConfig:
            config = castConfig
        }
        
        // 标准化和验证配置
        if err := config.Normalize(); err != nil {
            return nil, err
        }
        if err := config.Validate(); err != nil {
            return nil, err
        }
        
        // 应用配置（这里会调用各种管理器）
        configState, err := s.applyConfig(ctx, config, claim, results)
        if err != nil {
            return nil, err
        }
        
        preparedDeviceGroupConfigState[c] = configState
    }
    
    // ========================================
    // 步骤 5: 构建返回结果
    // ========================================
    var preparedDevices PreparedDevices
    for c, results := range configResultsMap {
        preparedDeviceGroup := PreparedDeviceGroup{
            ConfigState: *preparedDeviceGroupConfigState[c],
        }
        
        for _, result := range results {
            // 获取 CDI 设备 ID
            cdiDevices := []string{}
            if d := s.cdi.GetStandardDevice(s.allocatable[result.Device]); d != "" {
                cdiDevices = append(cdiDevices, d)
            }
            if d := s.cdi.GetClaimDevice(string(claim.UID), s.allocatable[result.Device], preparedDeviceGroupConfigState[c].containerEdits); d != "" {
                cdiDevices = append(cdiDevices, d)
            }
            
            device := &kubeletplugin.Device{
                Requests:     []string{result.Request},
                PoolName:     result.Pool,
                DeviceName:   result.Device,
                CDIDeviceIDs: cdiDevices,
            }
            
            // 根据设备类型构建 PreparedDevice
            var preparedDevice PreparedDevice
            switch s.allocatable[result.Device].Type() {
            case GpuDeviceType:
                preparedDevice.Gpu = &PreparedGpu{
                    Info:   s.allocatable[result.Device].Gpu,
                    Device: device,
                }
            // ... MIG 和 VFIO 类似
            }
            
            preparedDeviceGroup.Devices = append(preparedDeviceGroup.Devices, preparedDevice)
        }
        
        preparedDevices = append(preparedDevices, &preparedDeviceGroup)
    }
    
    return preparedDevices, nil
}
```

### 3.4 applyConfig() - 不同设备类型的处理

```go
// applyConfig 根据配置类型应用不同的配置策略
func (s *DeviceState) applyConfig(
    ctx context.Context,
    config configapi.Interface,
    claim *resourceapi.ResourceClaim,
    results []*resourceapi.DeviceRequestAllocationResult,
) (*DeviceConfigState, error) {
    
    switch castConfig := config.(type) {
    case *configapi.GpuConfig:
        return s.applyGpuConfig(ctx, castConfig, claim, results)
    case *configapi.MigDeviceConfig:
        return s.applyMigConfig(ctx, castConfig, claim, results)
    case *configapi.VfioDeviceConfig:
        return s.applyVfioConfig(ctx, castConfig, results)
    }
    
    return nil, fmt.Errorf("unsupported config type")
}

// applyGpuConfig 应用 GPU 配置
func (s *DeviceState) applyGpuConfig(
    ctx context.Context,
    config *configapi.GpuConfig,
    claim *resourceapi.ResourceClaim,
    results []*resourceapi.DeviceRequestAllocationResult,
) (*DeviceConfigState, error) {
    
    configState := &DeviceConfigState{}
    var devices PreparedDeviceList
    
    // 收集所有 GPU 设备
    for _, result := range results {
        allocatableDevice := s.allocatable[result.Device]
        devices = append(devices, PreparedDevice{
            Gpu: &PreparedGpu{Info: allocatableDevice.Gpu},
        })
    }
    
    // ========================================
    // 应用时间片配置
    // ========================================
    if featuregates.Enabled(featuregates.TimeSlicingSettings) {
        tsc := config.Sharing.TimeSlicingConfig
        if err := s.tsManager.SetTimeSlice(devices.Gpus(), tsc); err != nil {
            return nil, fmt.Errorf("set timeslice: %w", err)
        }
    }
    
    // ========================================
    // 启动 MPS 控制守护进程
    // ========================================
    if featuregates.Enabled(featuregates.MPSSupport) {
        if config.Sharing.Strategy == configapi.MpsSharing {
            mpsControlDaemon := s.mpsManager.NewMpsControlDaemon(
                string(claim.UID),
                &PreparedDeviceGroup{Devices: devices},
            )
            
            if err := mpsControlDaemon.Start(ctx, config.Sharing.MpsConfig); err != nil {
                return nil, fmt.Errorf("start MPS: %w", err)
            }
            
            configState.MpsControlDaemonID = mpsControlDaemon.ID()
            configState.containerEdits = mpsControlDaemon.ContainerEdits()
        }
    }
    
    return configState, nil
}

// applyVfioConfig 应用 VFIO 配置
func (s *DeviceState) applyVfioConfig(
    ctx context.Context,
    config *configapi.VfioDeviceConfig,
    results []*resourceapi.DeviceRequestAllocationResult,
) (*DeviceConfigState, error) {
    
    configState := &DeviceConfigState{}
    var devices PreparedDeviceList
    
    // 收集所有 VFIO 设备
    for _, result := range results {
        allocatableDevice := s.allocatable[result.Device]
        devices = append(devices, PreparedDevice{
            Vfio: &PreparedVfioDevice{Info: allocatableDevice.Vfio},
        })
    }
    
    // ========================================
    // 配置 VFIO 设备
    // ========================================
    if err := s.vfioPciManager.ConfigureDevices(devices.VfioDevices(), config); err != nil {
        return nil, fmt.Errorf("configure VFIO devices: %w", err)
    }
    
    return configState, nil
}
```

### 3.5 Checkpoint 原子性保证

```go
// updateCheckpoint 的原子性实现
func (s *DeviceState) updateCheckpoint(f func(*Checkpoint)) error {
    // 步骤 1: 读取当前 checkpoint
    checkpoint, err := s.getCheckpoint()
    if err != nil {
        return fmt.Errorf("unable to get checkpoint: %w", err)
    }
    
    // 步骤 2: 调用修改函数（内存操作）
    // s.Lock() 已在 Prepare() 中获取，保证内存原子性
    f(checkpoint)
    
    // 步骤 3: 写入文件（文件原子性）
    // CreateCheckpoint 内部使用临时文件 + os.Rename() 保证原子性
    if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFileBasename, checkpoint); err != nil {
        return fmt.Errorf("unable to create checkpoint: %w", err)
    }
    
    return nil
}

// CheckpointManager 的原子写入实现（简化）
func (cm *checkpointManager) CreateCheckpoint(name string, checkpoint Checkpoint) error {
    // 1. 序列化 checkpoint
    data, err := checkpoint.MarshalCheckpoint()
    if err != nil {
        return err
    }
    
    // 2. 写入临时文件
    tmpFile := filepath.Join(cm.path, name+".tmp")
    if err := os.WriteFile(tmpFile, data, 0600); err != nil {
        return err
    }
    
    // 3. 原子重命名（这是文件系统级别的原子操作）
    finalFile := filepath.Join(cm.path, name)
    if err := os.Rename(tmpFile, finalFile); err != nil {
        return err
    }
    
    // 4. fsync 确保持久化
    return fsync(filepath.Dir(finalFile))
}
```

---

## 阶段 4: 容器启动和设备使用

### 4.1 容器运行时集成流程

```
Kubelet
  │
  ├─► 1. 接收 Prepare 结果
  │     └─ CDI 设备 ID 列表：
  │         ["nvidia.com/gpu=gpu-0",
  │          "nvidia.com/gpu=my-gpu-claim-gpu-0"]
  │
  ├─► 2. 调用容器运行时（如 containerd）
  │     └─ 传递 CDI 设备 ID
  │
  │     ┌─────────────────────────────────────┐
  │     │      Container Runtime               │
  │     │                                      │
  │     ├─► 3. 解析 CDI 规范文件              │
  │     │     ├─ 标准规范：/etc/cdi/nvidia-gpu.yaml
  │     │     └─ Claim规范：/var/run/cdi/gpu-nvidia-com-my-gpu-claim-gpu-0.yaml
  │     │                                      │
  │     ├─► 4. 应用设备配置                   │
  │     │     ├─ 挂载设备文件（/dev/nvidia*）
  │     │     ├─ 设置环境变量（CUDA_VISIBLE_DEVICES）
  │     │     └─ 应用钩子（nvidia-cdi-hook）
  │     │                                      │
  │     └─► 5. 启动容器                       │
  │           └─ 容器内可访问 GPU 设备         │
  └────────────────────────────────────────────┘
```

### 4.2 CDI 规范文件示例

```yaml
# /etc/cdi/nvidia-gpu.yaml (标准规范)
cdiVersion: "0.3.0"
kind: "nvidia.com/gpu"
devices:
- name: gpu-0
  containerEdits:
    deviceNodes:
    - path: /dev/nvidia0
      type: c
      major: 195
      minor: 0
    - path: /dev/nvidiactl
      type: c
      major: 195
      minor: 255
    - path: /dev/nvidia-uvm
      type: c
      major: 235
      minor: 0
    - path: /dev/nvidia-uvm-tools
      type: c
      major: 235
      minor: 1
    mount:
    - hostPath: /usr/local/nvidia
      containerPath: /usr/local/nvidia
    env:
    - name: NVIDIA_DRIVER_CAPABILITIES
      value: compute,utility
```

```yaml
# /var/run/cdi/gpu-nvidia-com-my-gpu-claim-gpu-0.yaml (Claim规范)
cdiVersion: "0.3.0"
kind: "nvidia.com/gpu"
devices:
- name: my-gpu-claim-gpu-0
  containerEdits:
    env:
    - name: CUDA_VISIBLE_DEVICES
      value: "0"
    hooks:
    - hookName: prestart
      path: /usr/local/nvidia/nvidia-cdi-hook
      args:
      - prestart
      - --device-name=gpu-0
```

---

## 阶段 5: 资源清理 (Unprepare)

### 5.1 Unprepare 完整流程图

```
Kubelet
  │
  ├─► 1. Pod 终止或被删除
  │     └─ 需要清理 ResourceClaim "my-gpu-claim"
  │
  ├─► 2. 通过 gRPC 调用插件
  │     └─ UnprepareResourceClaims([claimRef])
  │
  │     ┌─────────────────────────────────────┐
  │     │      Plugin (driver.go)              │
  │     │                                      │
  │     ├─► UnprepareResourceClaims()         │
  │     │     │                                │
  │     │     └─► nodeUnprepareResource()     │
  │     │           │                          │
  │     │           ├─► 1. 获取 pulock        │
  │     │           │     └─ 10秒超时         │
  │     │           │                          │
  │     │           ├─► 2. state.Unprepare()  │
  │     │           │                          │
  │     │           └─► 3. 重新发布 ResourceSlice (Passthrough模式)
  │     └─────────────────────────────────────┘
  │
  └─► 3. 清理完成
        └─ 设备可重新分配
```

### 5.2 DeviceState.Unprepare() 详细流程

```go
// 函数调用链：
// kubelet → UnprepareResourceClaims() → nodeUnprepareResource() → state.Unprepare()

func (s *DeviceState) Unprepare(ctx context.Context, claimUID string) error {
    s.Lock()
    defer s.Unlock()
    
    // ========================================
    // 步骤 1: 从 Checkpoint 获取准备信息
    // ========================================
    checkpoint, err := s.getCheckpoint()
    if err != nil {
        return fmt.Errorf("unable to get checkpoint: %v", err)
    }
    
    pc, exists := checkpoint.V2.PreparedClaims[claimUID]
    if !exists {
        // Claim 不存在，可能是已经清理过了
        klog.Infof("unprepare noop: claim not found in checkpoint data: %v", claimUID)
        return nil
    }
    
    // ========================================
    // 步骤 2: 根据状态决定操作
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
    // 步骤 3: 恢复兄弟设备（Passthrough 模式）
    // ========================================
    if featuregates.Enabled(featuregates.PassthroughSupport) {
        for _, device := range pc.PreparedDevices.GetDevices() {
            allocatableDevice, ok := s.allocatable[device.DeviceName]
            if !ok {
                continue
            }
            err := s.discoverSiblingAllocatables(allocatableDevice)
            if err != nil {
                return fmt.Errorf("error discovering sibling allocatables: %w", err)
            }
        }
    }
    
    // ========================================
    // 步骤 4: 删除 claim 特定的 CDI 规范文件
    // ========================================
    if err := s.cdi.DeleteClaimSpecFile(claimUID); err != nil {
        return fmt.Errorf("unable to delete CDI spec file for claim: %w", err)
    }
    
    // ========================================
    // 步骤 5: 从 Checkpoint 删除记录
    // ========================================
    delete(checkpoint.V2.PreparedClaims, claimUID)
    if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFileBasename, checkpoint); err != nil {
        return fmt.Errorf("unable to sync to checkpoint: %v", err)
    }
    
    return nil
}
```

### 5.3 unprepareDevices() 核心逻辑

```go
// unprepareDevices 清理设备资源
func (s *DeviceState) unprepareDevices(ctx context.Context, claimUID string, devices PreparedDevices) error {
    // 遍历所有设备组
    for _, group := range devices {
        // ========================================
        // 步骤 1: 清理 VFIO 设备
        // ========================================
        if featuregates.Enabled(featuregates.PassthroughSupport) {
            err := s.unprepareVfioDevices(ctx, group.Devices.VfioDevices())
            if err != nil {
                return err
            }
        }
        
        // ========================================
        // 步骤 2: 停止 MPS 控制守护进程
        // ========================================
        if featuregates.Enabled(featuregates.MPSSupport) {
            mpsControlDaemon := s.mpsManager.NewMpsControlDaemon(claimUID, group)
            if err := mpsControlDaemon.Stop(ctx); err != nil {
                return fmt.Errorf("error stopping MPS control daemon: %w", err)
            }
        }
        
        // ========================================
        // 步骤 3: 恢复时间片配置
        // ========================================
        if featuregates.Enabled(featuregates.TimeSlicingSettings) {
