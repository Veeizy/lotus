package com.lotus.android

import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Semaphore
import kotlinx.coroutines.sync.withPermit
import java.net.Inet4Address
import java.net.InetSocketAddress
import java.net.Socket
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger

/**
 * 局域网服务端发现，两条路并行：
 * 1. NsdManager 发现 lotus 的 mDNS 服务（_picnet._udp，libp2p zeroconf 广播）拿到候选 IP；
 * 2. 本机所在 IPv4 网段（钳制 /24）并发 TCP 探测默认端口 3456/3466。
 * 命中的地址都再经 GET /api/net/info 验证 app=="picnet" 后回调。
 * 完成判定：子网相所有探测结束 + mDNS 相 6s 超时，两者都收口后 onDone 恰好一次。
 */
class LanScanner(context: Context) {
    companion object {
        /** lotus-server 默认 HTTP 3456 + 客户端(桌面版)默认 3466 */
        private val PORTS = intArrayOf(3456, 3466)
        private const val MDNS_TYPE = "_picnet._udp"
        private const val MDNS_WINDOW_MS = 6000L
        private const val CONNECT_TIMEOUT_MS = 350
        private const val PARALLEL = 64
    }

    private val appContext = context.applicationContext
    private val running = AtomicBoolean(false)
    private var scope: CoroutineScope? = null
    private val nsd: NsdManager? by lazy {
        try {
            appContext.getSystemService(Context.NSD_SERVICE) as NsdManager
        } catch (e: Exception) {
            null
        }
    }

    /** onFound 可能从子线程回调（已按 url 去重）；onDone 恰好一次。重复调用 scan 时忽略。 */
    fun scan(onFound: (ServerIdentity) -> Unit, onDone: () -> Unit) {
        if (!running.compareAndSet(false, true)) return
        val sc = CoroutineScope(SupervisorJob() + Dispatchers.IO)
        scope = sc
        val found = ConcurrentHashMap<String, Boolean>()
        val pending = AtomicInteger(2)

        val finishOne = {
            if (pending.decrementAndGet() == 0) {
                running.set(false)
                onDone()
            }
        }

        // 相 1：子网 TCP 扫描（无网络时空转，立即收口）
        val hosts = subnetHosts()
        if (hosts.isEmpty()) {
            finishOne()
        } else {
            sc.launch {
                val sem = Semaphore(PARALLEL)
                val jobs = hosts.flatMap { ip ->
                    PORTS.map { port ->
                        sc.launch {
                            sem.withPermit {
                                if (!running.get()) return@withPermit
                                if (!portOpen(ip, port)) return@withPermit
                                val url = "http://$ip:$port"
                                if (found.putIfAbsent(url, true) != null) return@withPermit
                                val id = ServerInfo.probe(url, 2000)
                                if (id != null) onFound(id) else found.remove(url)
                            }
                        }
                    }
                }
                jobs.forEach { it.join() }
                finishOne()
            }
        }

        // 相 2：mDNS 发现（固定窗口后收口；mDNS 不可用只损失候选 IP，不影响相 1）
        val mgr = nsd
        if (mgr == null) {
            finishOne()
        } else {
            val listener = object : NsdManager.DiscoveryListener {
                override fun onServiceFound(service: NsdServiceInfo) {
                    try {
                        mgr.resolveService(service, object : NsdManager.ResolveListener {
                            override fun onServiceResolved(info: NsdServiceInfo) {
                                val host = info.host?.hostAddress ?: return
                                sc.launch {
                                    for (port in PORTS) {
                                        if (!running.get()) return@launch
                                        val url = "http://$host:$port"
                                        if (found.putIfAbsent(url, true) != null) continue
                                        if (!portOpen(host, port)) { found.remove(url); continue }
                                        val id = ServerInfo.probe(url, 2000)
                                        if (id != null) onFound(id) else found.remove(url)
                                    }
                                }
                            }

                            override fun onResolveFailed(info: NsdServiceInfo, errorCode: Int) {}
                        })
                    } catch (e: Exception) { /* 单个解析失败忽略 */ }
                }

                override fun onServiceLost(service: NsdServiceInfo) {}
                override fun onDiscoveryStarted(serviceType: String) {}
                override fun onDiscoveryStopped(serviceType: String) {}
                override fun onStartDiscoveryFailed(serviceType: String, errorCode: Int) {
                    // 起播失败：无候选损失；完成计数由 6s 兜底协程收口
                }

                override fun onStopDiscoveryFailed(serviceType: String, errorCode: Int) {}
            }
            var mdnsStarted = false
            try {
                mgr.discoverServices(MDNS_TYPE, NsdManager.PROTOCOL_DNS_SD, listener)
                mdnsStarted = true
            } catch (e: Exception) {
            }
            if (mdnsStarted) {
                sc.launch {
                    delay(MDNS_WINDOW_MS)
                    try { mgr.stopServiceDiscovery(listener) } catch (e: Exception) {}
                    finishOne()
                }
            } else {
                finishOne()
            }
        }
    }

    fun cancel() {
        if (running.compareAndSet(true, false)) {
            scope?.cancel()
            scope = null
        }
    }

    private fun portOpen(host: String, port: Int): Boolean = try {
        Socket().use { s ->
            s.connect(InetSocketAddress(host, port), CONNECT_TIMEOUT_MS)
            true
        }
    } catch (e: Exception) {
        false
    }

    /** 本机所有 IPv4 网段的候选地址（每段钳制为含本机地址的 /24，避免蜂窝大网段）。 */
    private fun subnetHosts(): List<String> {
        val out = LinkedHashSet<String>()
        try {
            val cm = appContext.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
            for (network in cm.allNetworks) {
                val lp: LinkProperties = cm.getLinkProperties(network) ?: continue
                for (la in lp.linkAddresses) {
                    val addr = la.address
                    if (addr !is Inet4Address || addr.isLoopbackAddress || addr.isLinkLocalAddress) continue
                    if (la.prefixLength < 16) continue
                    val parts = (addr.hostAddress ?: continue).split('.').map { it.toInt() }
                    val base = parts[0].toString() + "." + parts[1] + "." + parts[2] + "."
                    for (h in 1..254) out.add(base + h)
                }
            }
        } catch (e: Exception) { /* 无网络时返回空 */ }
        return out.toList()
    }
}
