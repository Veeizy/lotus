package com.lotus.android

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

/** lotus 服务端身份（GET /api/net/info 的公开引导信息）。 */
data class ServerIdentity(val url: String, val id: String, val name: String)

object ServerInfo {
    /** 规范化用户输入：ip:port / host:port / 完整 URL → http://host:port */
    fun normalizeUrl(raw: String): String? {
        var s = raw.trim()
        if (s.isEmpty()) return null
        if (!s.startsWith("http://") && !s.startsWith("https://")) s = "http://$s"
        return try {
            val u = URL(s)
            if (u.host.isNullOrEmpty()) null
            else {
                val port = if (u.port > 0) ":${u.port}" else ""
                "http://${u.host}$port"
            }
        } catch (e: Exception) {
            null
        }
    }

    /**
     * 探测是否为 lotus 服务端（app=="picnet"）。
     * 返回 null = 不是 lotus 服务或不可达。
     */
    suspend fun probe(url: String, timeoutMs: Int = 2500): ServerIdentity? = withContext(Dispatchers.IO) {
        try {
            val conn = URL("$url/api/net/info").openConnection() as HttpURLConnection
            conn.connectTimeout = timeoutMs
            conn.readTimeout = timeoutMs
            conn.requestMethod = "GET"
            try {
                if (conn.responseCode != 200) return@withContext null
                val body = conn.inputStream.use { it.readBytes().decodeToString() }
                val obj = JSONObject(body)
                if (obj.optString("app") != "picnet") return@withContext null
                ServerIdentity(
                    url = url,
                    id = obj.optString("id"),
                    name = obj.optString("name").ifBlank { URL(url).host },
                )
            } finally {
                conn.disconnect()
            }
        } catch (e: Exception) {
            null
        }
    }
}
