package com.lotus.android

import android.content.Context
import android.content.SharedPreferences
import org.json.JSONArray
import org.json.JSONObject

/** 记忆的服务端（手动添加的 + 点开用过的扫描结果），按 lastUsed 倒序。 */
data class ServerEntry(
    val url: String,
    val name: String,
    val id: String,
    val lastUsed: Long,
)

/**
 * 持久化：SharedPreferences 存 JSON 数组 + 自动重连开关。
 * 只记「用过」的服务端：手动添加直接入库；扫描结果要点开一次才入（避免邻居刷屏）。
 */
class ServerStore(context: Context) {
    private val prefs: SharedPreferences =
        context.getSharedPreferences("lotus-servers", Context.MODE_PRIVATE)

    companion object {
        private const val KEY_LIST = "servers"
        private const val KEY_AUTO = "autoConnect"
        private const val KEY_LAST = "lastUrl"
    }

    fun list(): List<ServerEntry> {
        val raw = prefs.getString(KEY_LIST, null) ?: return emptyList()
        return try {
            val arr = JSONArray(raw)
            (0 until arr.length()).map { i ->
                val o = arr.getJSONObject(i)
                ServerEntry(
                    url = o.getString("url"),
                    name = o.optString("name"),
                    id = o.optString("id"),
                    lastUsed = o.optLong("lastUsed", 0),
                )
            }.sortedByDescending { it.lastUsed }
        } catch (e: Exception) {
            emptyList()
        }
    }

    fun get(url: String): ServerEntry? = list().find { it.url == url }

    /** 入库/更新（存在则刷新 name/id，lastUsed 由 touch 单独管理）。 */
    fun upsert(id: ServerIdentity) {
        val rest = list().filter { it.url != id.url }
        val prev = list().find { it.url == id.url }
        val entry = ServerEntry(id.url, id.name, id.id, prev?.lastUsed ?: 0L)
        save(rest + entry)
    }

    /** 标记使用（置顶 + 记 lastUrl 供自动重连）。 */
    fun touch(url: String) {
        val now = System.currentTimeMillis()
        val updated = list().map {
            if (it.url == url) it.copy(lastUsed = now) else it
        }
        save(updated)
        prefs.edit().putString(KEY_LAST, url).apply()
    }

    fun remove(url: String) {
        save(list().filter { it.url != url })
        if (prefs.getString(KEY_LAST, null) == url) prefs.edit().remove(KEY_LAST).apply()
    }

    fun rename(url: String, name: String) {
        save(list().map { if (it.url == url) it.copy(name = name) else it })
    }

    fun lastUrl(): String? = prefs.getString(KEY_LAST, null)

    var autoConnect: Boolean
        get() = prefs.getBoolean(KEY_AUTO, true)
        set(v) = prefs.edit().putBoolean(KEY_AUTO, v).apply()

    private fun save(entries: List<ServerEntry>) {
        val arr = JSONArray()
        for (e in entries) {
            arr.put(JSONObject().apply {
                put("url", e.url)
                put("name", e.name)
                put("id", e.id)
                put("lastUsed", e.lastUsed)
            })
        }
        prefs.edit().putString(KEY_LIST, arr.toString()).apply()
    }
}
