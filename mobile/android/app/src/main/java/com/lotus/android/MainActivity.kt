package com.lotus.android

import android.content.Intent
import android.os.Bundle
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AlertDialog
import androidx.appcompat.app.AppCompatActivity
import androidx.appcompat.widget.SwitchCompat
import androidx.recyclerview.widget.LinearLayoutManager
import androidx.recyclerview.widget.RecyclerView
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/**
 * 主页 = 服务端选择页。
 * 「我的服务端」：手动添加 + 用过即存的扫描结果，lastUsed 倒序，长按重命名/删除；
 * 「局域网发现」：实时扫描（mDNS + 子网端口探测），点击 = 存入我的服务端并进入。
 * 自动重连：开关开启时启动直连上次服务端（3s 超时，失败回落主页提示）。
 */
class MainActivity : AppCompatActivity() {

    private lateinit var store: ServerStore
    private lateinit var scanner: LanScanner
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)
    private var scanJob: Job? = null

    private lateinit var myList: RecyclerView
    private lateinit var scanList: RecyclerView
    private lateinit var myEmpty: TextView
    private lateinit var scanEmpty: TextView
    private lateinit var scanStatus: TextView
    private lateinit var btnScan: TextView
    private lateinit var swAuto: SwitchCompat

    private val myAdapter = ServerAdapter(fromScan = false)
    private val scanAdapter = ServerAdapter(fromScan = true)

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        store = ServerStore(this)
        scanner = LanScanner(this)

        myList = findViewById(R.id.my_list)
        scanList = findViewById(R.id.scan_list)
        myEmpty = findViewById(R.id.my_empty)
        scanEmpty = findViewById(R.id.scan_empty)
        scanStatus = findViewById(R.id.scan_status)
        btnScan = findViewById(R.id.btn_scan)
        swAuto = findViewById(R.id.sw_auto)

        myList.layoutManager = LinearLayoutManager(this)
        scanList.layoutManager = LinearLayoutManager(this)
        myList.adapter = myAdapter
        scanList.adapter = scanAdapter

        findViewById<View>(R.id.btn_add).setOnClickListener { showAddDialog() }
        btnScan.setOnClickListener { startScan() }
        swAuto.isChecked = store.autoConnect
        swAuto.setOnCheckedChangeListener { _, checked -> store.autoConnect = checked }

        refreshMyList()
        maybeAutoConnect()
    }

    override fun onResume() {
        super.onResume()
        refreshMyList()
        refreshOnlineDots()
    }

    override fun onDestroy() {
        scanner.cancel()
        scope.cancel()
        super.onDestroy()
    }

    private fun refreshMyList() {
        val entries = store.list()
        myAdapter.submit(entries)
        myEmpty.visibility = if (entries.isEmpty()) View.VISIBLE else View.GONE
        myList.visibility = if (entries.isEmpty()) View.GONE else View.VISIBLE
    }

    /** 后台探测「我的服务端」在线状态，刷新小圆点。 */
    private fun refreshOnlineDots() {
        val entries = store.list()
        if (entries.isEmpty()) return
        scope.launch {
            val online = entries.map { e -> e.url to (withTimeoutOrNull(3000) { ServerInfo.probe(e.url, 2500) } != null) }
            if (isFinishing || isDestroyed) return@launch
            myAdapter.setOnline(online.toMap())
        }
    }

    private fun maybeAutoConnect() {
        if (!store.autoConnect) return
        val last = store.lastUrl() ?: return
        val entry = store.get(last) ?: return
        scope.launch {
            val id = withTimeoutOrNull(3000) { ServerInfo.probe(entry.url, 2500) }
            if (id != null && !isFinishing) {
                openGallery(entry.url, entry.name)
            } else if (!isFinishing) {
                Toast.makeText(this@MainActivity,
                    "自动连接 ${entry.name} 失败，请手动选择", Toast.LENGTH_SHORT).show()
            }
        }
    }

    private fun openGallery(url: String, name: String) {
        store.touch(url)
        val i = Intent(this, GalleryActivity::class.java)
        i.putExtra(GalleryActivity.EXTRA_URL, url)
        i.putExtra(GalleryActivity.EXTRA_NAME, name)
        startActivity(i)
    }

    /* ---------------- 手动添加 ---------------- */

    private fun showAddDialog() {
        val input = EditText(this).apply {
            hint = "服务端地址，如 192.168.1.5:3456"
            setSingleLine()
        }
        val pad = (resources.displayMetrics.density * 20).toInt()
        val box = FrameLayout(this).apply {
            setPadding(pad, pad / 2, pad, 0)
            addView(input)
        }
        AlertDialog.Builder(this)
            .setTitle(R.string.add_server)
            .setView(box)
            .setPositiveButton("连接") { _, _ ->
                val url = ServerInfo.normalizeUrl(input.text.toString())
                if (url == null) {
                    Toast.makeText(this, "地址格式不对", Toast.LENGTH_SHORT).show()
                    return@setPositiveButton
                }
                scope.launch {
                    val id = withTimeoutOrNull(5000) { ServerInfo.probe(url) }
                    if (id == null) {
                        Toast.makeText(this@MainActivity, "未发现 lotus 服务端：$url", Toast.LENGTH_LONG).show()
                    } else {
                        store.upsert(id)
                        refreshMyList()
                        openGallery(id.url, id.name)
                    }
                }
            }
            .setNegativeButton(android.R.string.cancel, null)
            .show()
    }

    /* ---------------- 长按：重命名 / 删除 ---------------- */

    private fun showEntryMenu(entry: ServerEntry) {
        val items = arrayOf("重命名", "删除")
        AlertDialog.Builder(this)
            .setTitle(entry.name)
            .setItems(items) { _, which ->
                when (which) {
                    0 -> showRenameDialog(entry)
                    1 -> {
                        store.remove(entry.url)
                        refreshMyList()
                    }
                }
            }
            .show()
    }

    private fun showRenameDialog(entry: ServerEntry) {
        val input = EditText(this).apply {
            setText(entry.name)
            setSingleLine()
        }
        val pad = (resources.displayMetrics.density * 20).toInt()
        val box = FrameLayout(this).apply {
            setPadding(pad, pad / 2, pad, 0)
            addView(input)
        }
        AlertDialog.Builder(this)
            .setTitle("重命名")
            .setView(box)
            .setPositiveButton("保存") { _, _ ->
                val name = input.text.toString().trim()
                if (name.isNotEmpty()) {
                    store.rename(entry.url, name)
                    refreshMyList()
                }
            }
            .setNegativeButton(android.R.string.cancel, null)
            .show()
    }

    /* ---------------- 局域网扫描 ---------------- */

    private fun startScan() {
        if (scanJob?.isActive == true) return
        scanEmpty.visibility = View.GONE
        scanAdapter.submit(emptyList())
        btnScan.isEnabled = false
        btnScan.text = getString(R.string.scanning)
        scanStatus.visibility = View.VISIBLE
        scanStatus.text = "正在扫描局域网（mDNS + 端口探测）…"
        scanner.scan(
            onFound = { id ->
                runOnUiThread {
                    scanAdapter.add(id)
                    scanEmpty.visibility = View.GONE
                }
            },
            onDone = {
                runOnUiThread {
                    btnScan.isEnabled = true
                    btnScan.text = getString(R.string.scan)
                    scanStatus.visibility = View.GONE
                    scanEmpty.visibility = if (scanAdapter.itemCount == 0) View.VISIBLE else View.GONE
                }
            },
        )
    }

    /* ---------------- 列表适配器（我的 / 发现共用布局） ---------------- */

    private inner class ServerAdapter(val fromScan: Boolean) : RecyclerView.Adapter<ServerAdapter.VH>() {
        private val items = mutableListOf<Any>() // ServerEntry 或 ServerIdentity
        private val online = mutableMapOf<String, Boolean>()

        fun submit(list: List<ServerEntry>) {
            items.clear()
            items.addAll(list)
            online.clear()
            notifyDataSetChanged()
        }

        fun add(id: ServerIdentity) {
            if (items.any { (it as? ServerIdentity)?.url == id.url }) return
            items.add(id)
            notifyItemInserted(items.size - 1)
        }

        fun setOnline(map: Map<String, Boolean>) {
            online.clear()
            online.putAll(map)
            notifyDataSetChanged()
        }

        override fun onCreateViewHolder(parent: ViewGroup, viewType: Int): VH {
            val v = LayoutInflater.from(parent.context).inflate(R.layout.item_server, parent, false)
            return VH(v)
        }

        override fun getItemCount(): Int = items.size

        override fun onBindViewHolder(h: VH, position: Int) {
            val item = items[position]
            val isEntry = item is ServerEntry
            val url = if (isEntry) (item as ServerEntry).url else (item as ServerIdentity).url
            val name = if (isEntry) (item as ServerEntry).name else (item as ServerIdentity).name
            val id = if (isEntry) (item as ServerEntry).id else (item as ServerIdentity).id

            h.name.text = name
            h.sub.text = if (id.length > 16) "$url · ${id.take(12)}…" else "$url · $id"
            if (isEntry) {
                h.badge.visibility = View.VISIBLE
                h.badge.text = getString(R.string.last_used)
                val on = online[url]
                h.dot.setBackgroundResource(if (on == true) R.drawable.dot_green else R.drawable.dot_gray)
                h.dot.alpha = if (on == null) 0.4f else 1f
                h.itemView.setOnLongClickListener { showEntryMenu(item as ServerEntry); true }
            } else {
                h.badge.visibility = View.GONE
                h.dot.setBackgroundResource(R.drawable.dot_green)
                h.itemView.setOnLongClickListener(null)
            }
            h.itemView.setOnClickListener {
                when (val it2 = items[h.bindingAdapterPosition]) {
                    is ServerEntry -> openGallery(it2.url, it2.name)
                    is ServerIdentity -> {
                        store.upsert(it2)
                        refreshMyList()
                        openGallery(it2.url, it2.name)
                    }
                }
            }
        }

        inner class VH(v: View) : RecyclerView.ViewHolder(v) {
            val dot: View = v.findViewById(R.id.dot)
            val name: TextView = v.findViewById(R.id.name)
            val sub: TextView = v.findViewById(R.id.sub)
            val badge: TextView = v.findViewById(R.id.badge)
        }
    }
}
