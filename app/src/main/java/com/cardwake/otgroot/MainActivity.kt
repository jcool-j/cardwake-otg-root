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
import android.util.Base64
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
        webView.settings.allowFileAccess = true

        webView.webViewClient = object : WebViewClient() {
            override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? {
                return assetLoader.shouldInterceptRequest(request.url)
            }
        }

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
                val p = Runtime.getRuntime().exec(arrayOf("su", "-c", cmd))
                val stdout = p.inputStream.bufferedReader().readText()
                val stderr = p.errorStream.bufferedReader().readText()
                val code = p.waitFor()
                JSONObject().apply {
                    put("ok", code == 0)
                    put("code", code)
                    put("stdout", stdout)
                    put("stderr", stderr)
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
            val script = """
                for d in /sys/block/sd* /sys/block/mmcblk*; do
                  [ -e "${'$'}d" ] || continue
                  b=$(basename "${'$'}d")
                  [ "${'$'}b" = "mmcblk0" ] && continue
                  rem=$(cat "${'$'}d/removable" 2>/dev/null || echo 0)
                  ro=$(cat "${'$'}d/ro" 2>/dev/null || echo 0)
                  sec=$(cat "${'$'}d/size" 2>/dev/null || echo 0)
                  bytes=$((sec * 512))
                  model=$(cat "${'$'}d/device/model" "${'$'}d/device/name" 2>/dev/null | tr '
' ' ')
                  cid=$(cat "${'$'}d/device/cid" 2>/dev/null || echo "")
                  csd=$(cat "${'$'}d/device/csd" 2>/dev/null || echo "")
                  serial=$(cat "${'$'}d/device/serial" 2>/dev/null || echo "")
                  echo "${'$'}b|${'$'}bytes|${'$'}ro|${'$'}rem|${'$'}model|${'$'}cid|${'$'}csd|${'$'}serial"
                done
            """.trimIndent()
            val arr = JSONArray()
            try {
                val p = Runtime.getRuntime().exec(arrayOf("su", "-c", script))
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
                val cmd = "dd if=$devicePath bs=512 skip=${offset / 512} count=${(length + 511) / 512} 2>/dev/null"
                val p = Runtime.getRuntime().exec(arrayOf("su", "-c", cmd))
                val bytes = p.inputStream.readBytes()
                p.waitFor()
                if (bytes.isEmpty()) "" else Base64.encodeToString(bytes, 0, minOf(bytes.size, length), Base64.NO_WRAP)
            } catch (_: Exception) {
                ""
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
            val pi = PendingIntent.getBroadcast(
                this@MainActivity, 0, Intent(ACTION_USB_PERMISSION),
                PendingIntent.FLAG_IMMUTABLE
            )
            usbManager.requestPermission(dev, pi)
        }
    }

    override fun onDestroy() {
        unregisterReceiver(usbReceiver)
        super.onDestroy()
    }
}
