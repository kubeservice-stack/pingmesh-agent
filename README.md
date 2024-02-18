# Based on

## Background

Data centers themselves are highly complex, with networks involving numerous devices that further amplify this complexity. A large-scale data center consists of hundreds or even thousands of nodes, network interface cards, switches, routers, as well as countless network cables and optical fibers. On top of these hardware components, a plethora of software systems are built, such as search engines, distributed file systems, and distributed storage, among others. During the operation of these systems, various challenges arise: How can one determine whether a fault is a network issue? How can network SLAs (Service Level Agreements) be defined and tracked? What are the procedures for troubleshooting once a fault occurs?

![IDC](https://kubeservice.cn/img/devops/IDC_hu8ec2fdff58b0ea09e7358f84cbaf1df1_175984_filter_3454788233369042773.png)

Implementing `Network Performance Data Monitoring` is quite challenging. If we simply use the `ping` command to collect results, having each server ping the remaining `(N-1)` servers would result in a complexity of `N^2`, leading to both stability and performance issues.

For instance:
If there are 10,000 servers in the IDC, the task of pinging would involve `10,000 * 9999` tasks. If a single machine sends multiple IP requests, the workload doubles.

Data storage presents another issue. If pinging occurs every 30 seconds, with a payload size of 64 bytes per ping, the required data storage would be: `10,000 * 9999 * 2 * 64 * 24 * 3600 / 30` = `3.6860314e+13 bytes` = `33.52TB`.

Considering whether to only record `fail` and `timeout` instances could save `99.99%` of storage space.

## Production Implementation

This architecture is an `enhanced` implementation based on the `Microsoft Pingmesh paper`.

Original Microsoft Pingmesh paper link:
[Pingmesh: A Large-Scale System for Data Center Network Latency Measurement and Analysis](https://conferences.sigcomm.org/sigcomm/2015/pdf/papers/p139.pdf)

For monitoring networks, `Microsoft Pingmesh` serves as a significant breakthrough (details can be found in the original paper). However, there are several limitations in practical use:

1. Agent Data Flow: For each ping, the `Agent` records information into logs, which are then collected through the infrastructure for log data analysis. This process, involving a `log analysis` system, increases system complexity.

2. Ping Mode Support: Only supports `UDP` mode, lacking support for protocols like `DNS tcp` and `ICMP ping`.

3. Ping Dimensions: Supports only `IPv4` pinging. However, many scenarios require support for pinging involving aspects like public network connectivity and domain/DNS across the network.

4. Lack of Support for Manual Real-Time Ping Attempts: Achievable through network probing with `blackbox-exporter`.

5. Lack of IPv6 Support.

## Upgraded Pingmesh Architecture

![Pingmesh+](https://kubeservice.cn/img/devops/pingmesh_hu8c196f2563a4108ff3fa8682517063fd_177531_filter_4759638724306006349.png)
[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2Fkubeservice-stack%2Fpingmesh-agent.svg?type=shield)](https://app.fossa.com/projects/git%2Bgithub.com%2Fkubeservice-stack%2Fpingmesh-agent?ref=badge_shield)

### Controller

The `Controller` is primarily responsible for generating the `pinglist.yaml` file. The generation of the `pinglist` originates from three sources:

> Automatically acquiring the entire cluster's podIP and nodeIP list through the `IP Controller`.

> Configuring the `Agent Setting` through the `Pinglist Controller`.

> Supplementing external addresses in the `pinglist.yaml` file through `Custom Define Pinglist`. This includes support for `DNS addresses`, `external HTTP addresses`, `domain addresses`, `NTP addresses`, `Kubernetes API server addresses`, and more.

After generating the `pinglist` file, the `Controller` exposes it through `HTTP/HTTPS`. The `Agent` periodically retrieves the `pinglist` to update its own configuration, known as the 'pull' model. The `Controller` needs to ensure high availability, thus requiring multiple instances configured behind a `Service`. Each instance follows the same algorithm, and the content of the `pinglist` file remains consistent to ensure availability.

### Agent

Each ping action initiates a new connection to reduce the `TCP` concurrency caused by `Pingmesh`. The minimum ping interval between two servers is 10 seconds, and the maximum packet size is 64 KB.

```yaml
setting:
  # the maximum amount of concurrent to ping, uint
  concurrent_limit: 20
  # interval to exec ping in seconds, float
  interval: 60.0
  # The maximum delay time to ping in milliseconds, float
  delay: 200
  # ping timeout in seconds, float
  timeout: 2.0
  # send ip addr
  source_ip_addr: 0.0.0.0
  # send ip protocal
  ip_protocol: ip6

mesh:
  add-ping-public: 
    name: ping-public-demo
    type: OtherIP
    ips :
      - 127.0.0.1
      - 8.8.8.8
      - www.baidu.com
      - kubernetes.default.svc.cluster.local
```

Furthermore, `overload protection` is implemented:

1. If there is a large amount of data in the `pinglist`, and it cannot be processed within a single cycle (e.g., `10s`), it ensures that after completing the ongoing cycle, the next one will be prioritized to complete in a full rotation.
2. Configuration can set the concurrent thread count for the `Agent`, ensuring that the impact of `Pingmesh Agent` on the entire cluster remains below `one-thousandth`.
3. In the metrics, a `Prometheus Gauge` is utilized to calculate separately within each cycle.


```metrics 
# HELP ping_fail ping fail
# TYPE ping_fail gauge
ping_fail{target="8.8.8.8",tor="ping-public-demo"} 1
```

4. To ensure that ping requests are evenly distributed within a defined `time window interval`, memory-state calculations are applied to the request job. A `ratelimit` is implemented on concurrent coroutines to achieve this.

## Network Condition Design

Using the `interval` time window specified in the `pinglist.yaml` settings:
- Requests exceeding the `timeout` duration will be marked as `ping_fail`.
- Requests exceeding the `delay` but not the `timeout` duration will be marked as `ping_duration_milliseconds`.
- Requests not exceeding the `delay` will not be recorded in the metrics interface.

## Integration with Prometheus

Add the following text to the `scrape_configs` section of the `prometheus.yaml`, where `pingmeship` is the IP of the server.


```yaml
scrape_configs:

  - job_name: net_monitor
    honor_labels: true
    honor_timestamps: true
    scrape_interval: 60s
    scrape_timeout: 5s
    metrics_path: /metrics
    scheme: http
    static_configs:
    - targets:
      - $pingmeship:9115
```


## License
[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2Fkubeservice-stack%2Fpingmesh-agent.svg?type=large)](https://app.fossa.com/projects/git%2Bgithub.com%2Fkubeservice-stack%2Fpingmesh-agent?ref=badge_large)
