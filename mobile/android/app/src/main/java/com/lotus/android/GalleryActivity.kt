package com.lotus.android

import android.annotation.SuppressLint
import android.content.ContentValues
import android.net.Uri
import android.os.Bundle
import android.provider.MediaStore
import android.view.View
import android.webkit.JavascriptInterface
import android.webkit.MimeTypeMap
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.ProgressBar
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.webkit.WebViewAssetLoader
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import java.net.URLEncoder

/**
 * 图库页：本地打包的 TS 前端（assets/frontend，WebViewAssetLoader 挂到
 * https://appassets.androidplatform.net）经 ?server= 直连所选服务端的 HTTP API。
 * 纯消费者：LOAD_NO_CACHE 保证图片缓存只进内存；mixedContent=ALWAYS_ALLOW
 * 是 https 资产源访问 http 内网 API 的前提。
 * LotusAndroid.saveImage：长按图片保存到系统相册（原生下载，无 CORS 限制；
 * API 29+ 写 MediaStore 自己的条目免存储权限，故 minSdk=29）。
 */
class GalleryActivity : AppCompatActivity() {

    companion object {
        const val EXTRA_URL = "url"
        const val EXTRA_NAME = "name"
        private const val APP_ASSETS_ORIGIN = "https://appassets.androidplatform.net"
    }

    private lateinit var webView: WebView
    private lateinit var progress: ProgressBar
    private lateinit var errorBox: View
    private lateinit var errorText: TextView
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)
    private var serverUrl = ""

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_gallery)

        serverUrl = intent.getStringExtra(EXTRA_URL) ?: ""
        val name = intent.getStringExtra(EXTRA_NAME) ?: "服务端"
        title = name

        webView = findViewById(R.id.web)
        progress = findViewById(R.id.progress)
        errorBox = findViewById(R.id.error_box)
        errorText = findViewById(R.id.error_text)
        findViewById<View>(R.id.btn_retry).setOnClickListener { connect() }
        findViewById<View>(R.id.btn_back).setOnClickListener { finish() }

        // /assets/* → 资产 frontend/ 子目录（AssetsPathHandler 会剥掉注册前缀，
        // 必须显式补上 frontend/；否则 openAsset 找不到文件，返回 null 流 → ERR_INVALID_RESPONSE）
        val assetsHandler = WebViewAssetLoader.AssetsPathHandler(this)
        val assetLoader = WebViewAssetLoader.Builder()
            .addPathHandler("/assets/") { path -> assetsHandler.handle("frontend/$path") }
            .build()

        webView.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            mixedContentMode = WebSettings.MIXED_CONTENT_ALWAYS_ALLOW
            cacheMode = WebSettings.LOAD_NO_CACHE // 图库图片只进内存窗口，不落盘
            allowFileAccess = false
            allowContentAccess = false
        }
        webView.webViewClient = object : WebViewClient() {
            override fun shouldInterceptRequest(
                view: WebView,
                request: WebResourceRequest,
            ): WebResourceResponse? = assetLoader.shouldInterceptRequest(request.url)

            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                // 本地资产源内路由交给 WebView；其他源（理论不出现）交给系统
                if (request.url.host == "appassets.androidplatform.net") return false
                try {
                    startActivity(android.content.Intent(android.content.Intent.ACTION_VIEW, request.url))
                } catch (e: Exception) { /* 忽略 */ }
                return true
            }

            override fun onPageFinished(view: WebView, url: String) {
                progress.visibility = View.GONE
            }

            override fun onReceivedError(
                view: WebView,
                request: WebResourceRequest,
                error: android.webkit.WebResourceError,
            ) {
                if (!request.isForMainFrame) return
                showError("页面加载失败：${error.description}")
            }
        }

        webView.addJavascriptInterface(SaveBridge(), "LotusAndroid")

        connect()
    }

    private fun connect(showLoading: Boolean = true) {
        if (serverUrl.isEmpty()) {
            showError("缺少服务端地址")
            return
        }
        errorBox.visibility = View.GONE
        if (showLoading) progress.visibility = View.VISIBLE
        scope.launch {
            // 先探活再进页面，服务端不可达时给出明确错误而非白屏
            val id = withTimeoutOrNull(4000) { ServerInfo.probe(serverUrl, 3000) }
            if (id == null) {
                showError("无法连接服务端 $serverUrl\n请确认手机与服务端在同一网络、服务端已启动。")
                return@launch
            }
            val target = "$APP_ASSETS_ORIGIN/assets/index.html?server=" +
                URLEncoder.encode(serverUrl, "UTF-8")
            webView.loadUrl(target)
        }
    }

    private fun showError(msg: String) {
        progress.visibility = View.GONE
        errorText.text = getString(R.string.connect_failed) + "\n\n" + msg
        errorBox.visibility = View.VISIBLE
    }

    @Deprecated("Deprecated in Java")
    @Suppress("DEPRECATION")
    override fun onBackPressed() {
        // 优先级：全屏 → 仅退全屏（手势消费，不翻页）；有历史 → 上一页；否则离开回到服务端选择。
        // WebView 下 Fullscreen API 不可靠，前端以 fs-active 类为准（见 viewer.exitFullscreenIfActive）。
        webView.evaluateJavascript(
            "!!(window.appViewer && window.appViewer.exitFs && window.appViewer.exitFs())"
        ) { handled ->
            if (handled == "true") return@evaluateJavascript
            if (webView.canGoBack()) {
                webView.goBack()
            } else {
                super.onBackPressed()
            }
        }
    }

    override fun onDestroy() {
        scope.cancel()
        webView.destroy()
        super.onDestroy()
    }

    /**
     * 长按图片保存桥：前端传入图片的服务端 URL，原生下载后写入系统相册
     * （MediaStore Pictures/Lotus）。仅接受当前服务端的地址；写自己的媒体
     * 条目在 API 29+ 免存储权限。
     */
    private inner class SaveBridge {
        @JavascriptInterface
        fun saveImage(url: String?) {
            if (url.isNullOrEmpty() || !url.startsWith(serverUrl)) return
            try {
                val conn = URL(url).openConnection() as HttpURLConnection
                conn.connectTimeout = 5000
                conn.readTimeout = 120000
                try {
                    if (conn.responseCode !in 200..299) throw IOException("HTTP ${conn.responseCode}")
                    val mime = conn.contentType?.substringBefore(';')?.trim() ?: ""
                    val name = fileNameFor(url, if (mime.isNotEmpty()) mime else "image/jpeg")
                    val values = ContentValues().apply {
                        put(MediaStore.Images.Media.DISPLAY_NAME, name)
                        put(MediaStore.Images.Media.MIME_TYPE, mime.ifEmpty { "image/jpeg" })
                        put(MediaStore.Images.Media.RELATIVE_PATH, "Pictures/Lotus")
                    }
                    val uri: Uri = contentResolver
                        .insert(MediaStore.Images.Media.EXTERNAL_CONTENT_URI, values)
                        ?: throw IOException("无法写入系统相册")
                    contentResolver.openOutputStream(uri)?.use { out ->
                        conn.inputStream.use { it.copyTo(out) }
                    } ?: throw IOException("无法打开输出流")
                    runOnUiThread {
                        Toast.makeText(this@GalleryActivity, "已保存到相册：Pictures/Lotus/$name", Toast.LENGTH_SHORT).show()
                    }
                } finally {
                    conn.disconnect()
                }
            } catch (e: Exception) {
                runOnUiThread {
                    Toast.makeText(this@GalleryActivity, "保存失败：${e.message}", Toast.LENGTH_LONG).show()
                }
            }
        }
    }

    /** 文件名取自图片 API 的 p 参数（图库内路径）末段；无扩展名时按 MIME 补。 */
    private fun fileNameFor(url: String, mime: String): String {
        val raw = try { Uri.parse(url).getQueryParameter("p").orEmpty() } catch (e: Exception) { "" }
        var name = raw.substringAfterLast('\\').substringAfterLast('/').trim()
        if (name.isEmpty()) name = "lotus-${System.currentTimeMillis()}"
        if (!name.contains('.')) {
            val ext = MimeTypeMap.getSingleton().getExtensionFromMimeType(mime) ?: "jpg"
            name += ".$ext"
        }
        return name
    }
}
