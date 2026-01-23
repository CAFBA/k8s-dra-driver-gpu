# NVIDIA DRA Driver GPU - 项目文件结构与职责

## cmd/ - 可执行程序

### cmd/gpu-kubelet-plugin/ - GPU Kubelet插件

| 文件 | 主要职责 |
|------|---------|
| main.go | 程序入口、CLI初始化、启动插件 |
| driver.go | DRA接口实现(Prepare/Unprepare/Publish) |
| device_state.go | **核心状态管理**：设备分配跟踪、checkpoint持久化 |
| allocatable.go | 可分配设备集合管理(GPU/MIG/VFIO) |
| deviceinfo.go | 设备信息数据结构定义 |
| nvlib.go | NVML库集成、GPU设备枚举 |
| prepared.go | 已准备设备跟踪 |
| cdi.go | CDI规范文件生成 |
| cdioptions.go | CDI生成器配置 |
| checkpoint.go | checkpoint数据结构与持久化 |
| checkpointv.go | checkpoint版本兼容性处理 |
| health.go | HTTP健康检查服务 |
| device_health.go | NVML设备健康监控(XID错误) |
| sharing.go | GPU共享实现(TimeSlicing/MPS) |
| vfio-device.go | VFIO-PCI设备管理 |
| mutex.go | 跨调用互斥锁 |
| types.go | 设备类型常量定义 |
| root.go | 驱动根目录管理 |

### cmd/compute-domain-kubelet-plugin/ - ComputeDomain Kubelet插件

| 文件 | 主要职责 |
|------|---------|
| main.go | 程序入口 |
| driver.go | DRA接口实现 |
| device_state.go | ComputeDomain状态管理 |
| computedomain.go | ComputeDomain资源操作 |
| cdi.go | IMEX相关CDI生成 |
| nvlib.go | NVIDIA库集成 |
| cleanup.go | 资源清理 |

### cmd/compute-domain-controller/ - ComputeDomain控制器

| 文件 | 主要职责 |
|------|---------|
| main.go | 控制器入口 |
| controller.go | 主控制循环 |
| computedomain.go | ComputeDomain协调逻辑 |
| daemonset.go | DaemonSet管理 |
| mnsdaemonset.go | MNS DaemonSet管理 |
| node.go | Node资源处理 |
| indexers.go | 缓存索引器 |

### cmd/compute-domain-daemon/ - ComputeDomain守护进程

| 文件 | 主要职责 |
|------|---------|
| main.go | 守护进程入口 |
| controller.go | 本地控制逻辑 |
| computedomain.go | ComputeDomain处理 |
| podmanager.go | Pod管理 |
| process.go | IMEX进程管理 |

### cmd/webhook/ - 验证Webhook

| 文件 | 主要职责 |
|------|---------|
| main.go | Webhook服务器 |
| resource.go | 资源验证逻辑 |

## api/nvidia.com/resource/v1beta1/ - API定义

| 文件 | 主要职责 |
|------|---------|
| api.go | API组定义、编解码器初始化 |
| gpuconfig.go | GpuConfig CRD定义 |
| migconfig.go | MIG设备配置 |
| vfiodeviceconfig.go | VFIO设备配置 |
| sharing.go | GPU共享策略定义(TimeSlicing/MPS) |
| computedomain.go | ComputeDomain CRD定义 |
| computedomainconfig.go | ComputeDomain配置结构 |
| validate.go | 配置验证函数 |
| register.go | Scheme注册 |
| zz_generated.deepcopy.go | 自动生成的DeepCopy方法 |

## pkg/ - 公共包

### pkg/featuregates/

| 文件 | 主要职责 |
|------|---------|
| featuregates.go | 特性开关定义与管理(TimeSlicing/MPS/Passthrough等) |

### pkg/flags/

| 文件 | 主要职责 |
|------|---------|
| kubeclient.go | Kubernetes客户端配置 |
| logging.go | 日志配置 |
| featuregates.go | 特性开关CLI集成 |

### pkg/flock/

| 文件 | 主要职责 |
|------|---------|
| flock.go | 文件锁实现(prepare/unprepare互斥) |

### pkg/nvidia.com/

| 目录 | 主要职责 |
|------|---------|
| clientset/ | 自动生成的Kubernetes客户端 |
| informers/ | 自动生成的资源监听器 |
| listers/ | 自动生成的资源列表器 |

### pkg/workqueue/

| 文件 | 主要职责 |
|------|---------|
| workqueue.go | 控制器工作队列 |

## internal/ - 内部包

| 文件 | 主要职责 |
|------|---------|
| common/util.go | 通用工具函数 |
| common/nvcaps.go | NVIDIA capabilities处理 |
| info/version.go | 版本信息 |

## 学习建议

### GPU Kubelet Plugin核心学习顺序：

1. **入口与类型** (基础)
   - `main.go` - 理解程序启动
   - `types.go` - 理解设备类型
   - `deviceinfo.go` - 理解设备数据结构

2. **设备发现** (基础)
   - `nvlib.go` - GPU枚举
   - `allocatable.go` - 设备集合管理

3. **DRA核心** (重点)
   - `driver.go` - DRA接口实现
   - `device_state.go` - **最重要**：状态管理核心

4. **容器集成** (重要)
   - `cdi.go` - CDI文件生成
   - `prepared.go` - 已准备设备

5. **高级特性** (进阶)
   - `sharing.go` - GPU共享
   - `vfio-device.go` - VFIO直通
   - `device_health.go` - 健康监控

6. **持久化** (补充)
   - `checkpoint.go` - 状态持久化

### 关键概念：

- **DRA流程**: ResourceClaim → Prepare → CDI注入 → 容器启动 → Unprepare
- **设备类型**: GPU、MIG、VFIO
- **共享策略**: TimeSlicing、MPS
- **状态持久化**: Checkpoint机制