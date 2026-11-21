package com.cardwake.otgroot

import android.annotation.SuppressLint
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.hardware.usb.UsbDevice
import android.hardware.usb.UsbManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.content.ContentValues
import android.os.Environment
import android.provider.MediaStore
import android.util.Base64
import android.webkit.DownloadListener
import android.widget.Toast
import java.io.File
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread
import android.webkit.JavascriptInterface
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.webkit.WebViewAssetLoader
import org.json.JSONArray
import org.json.JSONObject
import java.io.BufferedReader
import java.io.InputStreamReader

class MainActivity : AppCompatActivity() {

    private lateinit var webView: WebView
    private lateinit var usbManager: UsbManager
    private var filePathCallback: ValueCallback<Array<Uri>>? = null
    private val ACTION_USB_PERMISSION = "com.cardwake.otgroot.USB_PERMISSION"

    private val fileChooser = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
        val data = result.data
        val uris = WebChromeClient.FileChooserParams.parseResult(result.resultCode, data)
        filePathCallback?.onReceiveValue(uris)
        filePathCallback = null
    }

    private val usbReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            if (intent.action == ACTION_USB_PERMISSION) {
                val granted = intent.getBooleanExtra(UsbManager.EXTRA_PERMISSION_GRANTED, false)
                webView.evaluateJavascript("window.dispatchEvent(new CustomEvent('cardwake-otg-perm',{detail:$granted}))", null)
            }
        }
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        usbManager = getSystemService(Context.USB_SERVICE) as UsbManager

        val filter = IntentFilter(ACTION_USB_PERMISSION)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            registerReceiver(usbReceiver, filter, RECEIVER_NOT_EXPORTED)
        } else {
            registerReceiver(usbReceiver, filter)
        }

        val assetLoader = WebViewAssetLoader.Builder()
            .addPathHandler("/assets/", WebViewAssetLoader.AssetsPathHandler(this))
            .build()

        webView = WebView(this)
        webView.settings.javaScriptEnabled = true
        webView.settings.domStorageEnabled = true
        webView.settings.allowFileAccess = false
        webView.settings.allowContentAccess = false
        webView.settings.javaScriptCanOpenWindowsAutomatically = false

        webView.webViewClient = object : WebViewClient() {
            override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? {
                return assetLoader.shouldInterceptRequest(request.url)
            }

            // Keep the WebView (and its root bridge) locked to our own offline origin.
            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                val u = request.url
                if (u.scheme == "https" && u.host == "appassets.androidplatform.net") return false
                if (u.scheme == "blob" || u.scheme == "data") return false
                return true
            }
        }

        webView.setDownloadListener(DownloadListener { url, _, contentDisposition, mimeType, _ ->
            val name = Uri.decode(
                Regex("filename\\*?=(?:UTF-8'')?\"?([^\";]+)").find(contentDisposition ?: "")?.groupValues?.get(1)
                    ?: "cardwake-export.bin"
            )
            val mime = mimeType ?: "application/octet-stream"
            when {
                url.startsWith("data:") -> {
                    val comma = url.indexOf(',')
                    val isB64 = comma > 0 && url.substring(0, comma).endsWith(";base64")
                    val payload = url.substring(comma + 1)
                    val bytes = if (isB64) Base64.decode(payload, Base64.DEFAULT) else Uri.decode(payload).toByteArray()
                    saveToDownloads(name, mime, bytes)
                }
                url.startsWith("blob:") -> webView.evaluateJavascript(
                    "(async()=>{const b=await (await fetch('" + url + "')).blob();" +
                        "const r=new FileReader();r.onloadend=()=>CardwakeAndroid.saveBase64File(" +
                        JSONObject.quote(name) + "," + JSONObject.quote(mime) + ",String(r.result).split(',')[1]||'');" +
                        "r.readAsDataURL(b);})()", null
                )
            }
        })

        webView.webChromeClient = object : WebChromeClient() {
            override fun onShowFileChooser(
                webView: WebView?,
                callback: ValueCallback<Array<Uri>>?,
                params: FileChooserParams?
            ): Boolean {
                filePathCallback?.onReceiveValue(null)
                filePathCallback = callback
                val intent = params?.createIntent() ?: return false
                fileChooser.launch(intent)
                return true
            }
        }

        webView.addJavascriptInterface(AndroidOtgRootBridge(), "CardwakeAndroid")
        setContentView(webView)

        // Served offline via HTTPS secure context so WebCrypto AES-256 & ES Modules work 100% offline
        webView.loadUrl("https://appassets.androidplatform.net/assets/www/index.html")
    }

    inner class AndroidOtgRootBridge {
        private fun isSafeExternalBlock(path: String): Boolean {
            return path.matches(Regex("^/dev/block/(sd[b-z][0-9]*|mmcblk[1-9][0-9]*(p[0-9]+)?)$"))
        }

        @JavascriptInterface
        fun hasRoot(): Boolean {
            return try {
                val p = Runtime.getRuntime().exec(arrayOf("su", "-c", "id -u"))
                val out = BufferedReader(InputStreamReader(p.inputStream)).readLine()?.trim()
                p.waitFor()
                out == "0"
            } catch (_: Exception) {
                false
            }
        }

        @JavascriptInterface
        fun execRoot(cmd: String): String {
            return try {
                val p = ProcessBuilder("su", "-c", cmd).start()
                p.outputStream.close()
                // Drain stderr on its own thread so neither pipe can fill up and deadlock the process.
                val errBuf = StringBuilder()
                val errT = thread(start = true) { errBuf.append(p.errorStream.bufferedReader().readText()) }
                val outT = StringBuilder()
                val outR = thread(start = true) { outT.append(p.inputStream.bufferedReader().readText()) }
                val finished = p.waitFor(120, TimeUnit.SECONDS)
                if (!finished) {
                    p.destroyForcibly()
                    errT.join(1000); outR.join(1000)
                    return JSONObject().put("ok", false).put("error", "Timed out after 120s").toString()
                }
                errT.join(5000); outR.join(5000)
                val code = p.exitValue()
                JSONObject().apply {
                    put("ok", code == 0)
                    put("code", code)
                    put("stdout", outT.toString())
                    put("stderr", errBuf.toString())
                }.toString()
            } catch (e: Exception) {
                JSONObject().apply {
                    put("ok", false)
                    put("error", e.message ?: "su failed")
                }.toString()
            }
        }

        @JavascriptInterface
        fun scanBlockDevices(): String {
            // Script lives in assets/ so the shell's $ variables are not mangled by Kotlin string templates.
            val script = assets.open("scan_block_devices.sh").bufferedReader().readText()
            val arr = JSONArray()
            try {
                val p = Runtime.getRuntime().exec(arrayOf("su", "-c", script))
                p.outputStream.close()
                val lines = p.inputStream.bufferedReader().readLines()
                p.waitFor()
                for (line in lines) {
                    val parts = line.split("|")
                    if (parts.size < 4) continue
                    val name = parts[0].trim()
                    val size = parts[1].trim().toLongOrNull() ?: 0L
                    val ro = parts[2].trim() == "1"
                    val model = parts.getOrNull(4)?.trim().orEmpty().ifEmpty { name }
                    arr.put(JSONObject().apply {
                        put("id", "root-$name")
                        put("device", "/dev/block/$name")
                        put("name", model)
                        put("size", size)
                        put("readonly", ro)
                        put("removable", parts[3].trim() == "1")
                        put("bus", if (name.startsWith("mmcblk")) "SD/MMC" else "USB-OTG")
                        put("cid", parts.getOrNull(5)?.trim().orEmpty())
                        put("csd", parts.getOrNull(6)?.trim().orEmpty())
                        put("serial", parts.getOrNull(7)?.trim().orEmpty())
                    })
                }
            } catch (_: Exception) {}
            return arr.toString()
        }

        @JavascriptInterface
        fun readBlockBase64(devicePath: String, offset: Long, length: Int): String {
            if (!isSafeExternalBlock(devicePath) || offset < 0 || length <= 0 || length > 1048576) return ""
            return try {
                // dd works in whole sectors: read from the sector boundary, then slice off the head padding.
                val pad = (offset % 512).toInt()
                val skip = offset / 512
                val count = (pad + length + 511) / 512
                val cmd = "dd if=$devicePath bs=512 skip=$skip count=$count 2>/dev/null"
                val p = Runtime.getRuntime().exec(arrayOf("su", "-c", cmd))
                p.outputStream.close()
                val bytes = p.inputStream.readBytes()
                p.waitFor()
                if (bytes.size <= pad) "" else Base64.encodeToString(bytes, pad, minOf(bytes.size - pad, length), Base64.NO_WRAP)
            } catch (_: Exception) {
                ""
            }
        }

        @JavascriptInterface
        fun saveBase64File(name: String, mime: String, b64: String): String {
            return try {
                val ok = saveToDownloads(name, mime, Base64.decode(b64, Base64.DEFAULT))
                JSONObject().put("ok", ok).toString()
            } catch (e: Exception) {
                JSONObject().put("ok", false).put("error", e.message ?: "save failed").toString()
            }
        }

        @JavascriptInterface
        fun writeBlockBase64(devicePath: String, offset: Long, base64Data: String): String {
            if (!isSafeExternalBlock(devicePath) || offset < 0 || offset % 512 != 0L) {
                return JSONObject().put("ok", false).put("error", "Unsafe path or unaligned offset").toString()
            }
            return try {
                val raw = Base64.decode(base64Data, Base64.DEFAULT)
                val cmd = "dd of=$devicePath bs=512 seek=${offset / 512} conv=notrunc 2>&1"
                val p = Runtime.getRuntime().exec(arrayOf("su", "-c", cmd))
                p.outputStream.write(raw)
                p.outputStream.flush()
                p.outputStream.close()
                val code = p.waitFor()
                JSONObject().put("ok", code == 0).toString()
            } catch (e: Exception) {
                JSONObject().put("ok", false).put("error", e.message ?: "write error").toString()
            }
        }

        @JavascriptInterface
        fun listOtgDevices(): String {
            val arr = JSONArray()
            usbManager.deviceList.values.forEach { dev ->
                arr.put(JSONObject().apply {
                    put("deviceName", dev.deviceName)
                    put("vid", String.format("%04X", dev.vendorId))
                    put("pid", String.format("%04X", dev.productId))
                    put("product", dev.productName ?: "USB OTG Device")
                    put("manufacturer", dev.manufacturerName ?: "")
                    put("hasPermission", usbManager.hasPermission(dev))
                })
            }
            return arr.toString()
        }

        @JavascriptInterface
        fun requestOtgPermission(deviceName: String) {
            val dev: UsbDevice = usbManager.deviceList[deviceName] ?: return
            // The system fills in EXTRA_PERMISSION_GRANTED, so on Android 12+ the PendingIntent must be mutable.
            val flags = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) PendingIntent.FLAG_MUTABLE else 0
            val pi = PendingIntent.getBroadcast(
                this@MainActivity, 0, Intent(ACTION_USB_PERMISSION).setPackage(packageName), flags
            )
            usbManager.requestPermission(dev, pi)
        }
    }

    private fun saveToDownloads(name: String, mime: String, bytes: ByteArray): Boolean {
        val safe = name.replace(Regex("[^A-Za-z0-9._ -]"), "_").ifEmpty { "cardwake-export.bin" }
        return try {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                val cv = ContentValues().apply {
                    put(MediaStore.Downloads.DISPLAY_NAME, safe)
                    put(MediaStore.Downloads.MIME_TYPE, mime)
                    put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS)
                }
                val uri = contentResolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, cv) ?: return false
                contentResolver.openOutputStream(uri)?.use { it.write(bytes) } ?: return false
            } else {
                val dir = getExternalFilesDir(Environment.DIRECTORY_DOWNLOADS) ?: filesDir
                File(dir, safe).writeBytes(bytes)
            }
            runOnUiThread { Toast.makeText(this, "Saved to Downloads: $safe", Toast.LENGTH_LONG).show() }
            true
        } catch (e: Exception) {
            runOnUiThread { Toast.makeText(this, "Save failed: ${e.message}", Toast.LENGTH_LONG).show() }
            false
        }
    }

    override fun onDestroy() {
        try { unregisterReceiver(usbReceiver) } catch (_: IllegalArgumentException) {}
        super.onDestroy()
    }
}
