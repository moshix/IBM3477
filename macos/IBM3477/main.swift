// IBM 3477 — native macOS app for the emulated InfoWindow terminal.
//
// The emulator and its web page stay in Go (cmd/appwebibm3477). This app
// replaces the browser tab: it starts the Go program as a hidden child in
// -embedded mode, reads the page URL from its stdout and shows the page in
// a WKWebView inside a normal Cocoa window with Dock icon and menus.
//
//   IBM3477.app/Contents/MacOS/
//     IBM3477          this program (AppKit)
//     appwebibm3477    the Go server, started as  appwebibm3477 -embedded
//
// Lifetime: the child's stdin is a pipe whose write end only this process
// holds. When the app quits, crashes or is killed the kernel closes it, the
// server sees EOF and shuts down (embedded.go) — no orphaned servers.
//
// What a browser did for the page and WKWebView does not do on its own is
// done here: downloads (print jobs saved as PDF/text) go through a Save
// panel, the Edit menu provides Cmd+V paste into the page's paste field,
// and window.open (the "open in browser" print choice) is refused so the
// page falls back to a download.
//
// Build: tools/make-macos-app.bash

import AppKit
@preconcurrency import WebKit

/// Name of the Go server inside Contents/MacOS, and where to look for it
/// when the bundle does not carry its own copy.
let serverName = "appwebibm3477"
let serverFallback = "/usr/local/bin/appwebibm3477"

/// The stdout line of the server that carries the page URL (embedded.go).
let urlPrefix = "APPURL "

/// How long the server may take to print its URL before we give up.
let startupTimeout: TimeInterval = 30

// MARK: - Server process

/// Runs appwebibm3477 -embedded and reports its URL or its failure.
final class Server {
    private let process = Process()
    private let stdinPipe = Pipe()
    private let stdoutPipe = Pipe()
    private let stderrPipe = Pipe()
    /// The last part of the server's stderr, shown if it fails.
    private var errText = ""
    private let lock = NSLock()
    /// Set once the app is quitting, so the child's exit is not an error.
    var stopping = false

    /// Called on the main thread with the page URL.
    var onReady: ((URL) -> Void)?
    /// Called on the main thread when the server ends unexpectedly, with
    /// its recent error output.
    var onExit: ((String) -> Void)?

    /// The server binary: the copy in the bundle, else /usr/local/bin.
    static func executable() -> URL? {
        if let u = Bundle.main.url(forAuxiliaryExecutable: serverName),
           FileManager.default.isExecutableFile(atPath: u.path) {
            return u
        }
        if FileManager.default.isExecutableFile(atPath: serverFallback) {
            return URL(fileURLWithPath: serverFallback)
        }
        return nil
    }

    func start() throws {
        guard let exe = Server.executable() else {
            throw NSError(domain: "IBM3477", code: 1, userInfo: [
                NSLocalizedDescriptionKey:
                    "\(serverName) not found in the app or in \(serverFallback)."])
        }
        process.executableURL = exe
        process.arguments = ["-embedded"]
        // Dock apps start with cwd "/"; the server needs none, but $HOME is
        // a friendlier place for anything relative.
        process.currentDirectoryURL = FileManager.default.homeDirectoryForCurrentUser
        process.standardInput = stdinPipe
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe

        collectStderr()
        readURL()
        process.terminationHandler = { [weak self] p in
            guard let self = self else { return }
            DispatchQueue.main.async {
                if self.stopping { return }
                self.lock.lock()
                var msg = self.errText.trimmingCharacters(in: .whitespacesAndNewlines)
                self.lock.unlock()
                if msg.isEmpty { msg = "exit status \(p.terminationStatus)" }
                self.onExit?(msg)
            }
        }
        try process.run()

        // No URL in time: report it like a failed start.
        DispatchQueue.main.asyncAfter(deadline: .now() + startupTimeout) { [weak self] in
            guard let self = self, !self.ready, !self.stopping else { return }
            self.stopping = true
            self.process.terminate()
            self.onExit?("The terminal server did not start within \(Int(startupTimeout)) s.")
        }
    }

    private var ready = false

    /// Reads stdout line by line until the URL line; the rest is ignored
    /// but still drained so the server never blocks on a full pipe.
    private func readURL() {
        let fh = stdoutPipe.fileHandleForReading
        DispatchQueue.global().async { [weak self] in
            var buf = Data()
            var found = false
            while true {
                let chunk = fh.availableData // blocks; empty = EOF
                if chunk.isEmpty { return }
                if found { continue }
                buf.append(chunk)
                // split off complete lines; keep a partial last line
                while let nl = buf.firstIndex(of: 0x0A) {
                    let line = String(decoding: buf[buf.startIndex..<nl], as: UTF8.self)
                    buf.removeSubrange(buf.startIndex...nl)
                    if line.hasPrefix(urlPrefix), let u = URL(string: String(line.dropFirst(urlPrefix.count))) {
                        found = true
                        buf.removeAll()
                        DispatchQueue.main.async {
                            guard let self = self, !self.stopping else { return }
                            self.ready = true
                            self.onReady?(u)
                        }
                        break
                    }
                }
            }
        }
    }

    /// Keeps the last ~4 KB of stderr for error messages.
    private func collectStderr() {
        stderrPipe.fileHandleForReading.readabilityHandler = { [weak self] fh in
            let d = fh.availableData
            guard let self = self else { return }
            if d.isEmpty { fh.readabilityHandler = nil; return }
            self.lock.lock()
            self.errText += String(decoding: d, as: UTF8.self)
            if self.errText.count > 4096 { self.errText = String(self.errText.suffix(4096)) }
            self.lock.unlock()
        }
    }

    /// Asks the server to stop (SIGTERM; closing stdin does the same if
    /// this process dies before it gets here).
    func stop() {
        stopping = true
        try? stdinPipe.fileHandleForWriting.close()
        if process.isRunning { process.terminate() }
    }
}

// MARK: - App

final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate,
    WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate {

    var window: NSWindow!
    var webView: WKWebView!
    let server = Server()
    /// The server's host:port; links elsewhere open in the default browser.
    var serverHost = ""
    var titleObservation: NSKeyValueObservation?

    func applicationDidFinishLaunching(_ note: Notification) {
        buildMenu()
        buildWindow()
        server.onReady = { [weak self] url in self?.load(url) }
        server.onExit = { [weak self] msg in self?.fail(msg) }
        do {
            try server.start()
        } catch {
            fail(error.localizedDescription)
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ app: NSApplication) -> Bool { true }

    func applicationWillTerminate(_ note: Notification) { server.stop() }

    // MARK: window and web view

    func buildWindow() {
        let cfg = WKWebViewConfiguration()
        // the terminal beeps/clicks through Web Audio after the first key
        cfg.mediaTypesRequiringUserActionForPlayback = []
        // Web Inspector via right click > Inspect Element, for debugging
        cfg.preferences.setValue(true, forKey: "developerExtrasEnabled")
        // mark the page as running in this app, before its CSS applies:
        // style.css then sizes the toolbar for a window without browser
        // chrome (html.native-app rules)
        cfg.userContentController.addUserScript(WKUserScript(
            source: "document.documentElement.classList.add('native-app')",
            injectionTime: .atDocumentStart, forMainFrameOnly: true))

        webView = WKWebView(frame: .zero, configuration: cfg)
        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.allowsMagnification = false // the page has its own zoom

        window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 1280, height: 820),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered, defer: false)
        window.title = "IBM 3477"
        window.contentView = webView
        window.delegate = self
        window.collectionBehavior = [.fullScreenPrimary]
        window.minSize = NSSize(width: 640, height: 400)
        window.center()
        // remembers size and position between runs
        window.setFrameAutosaveName("IBM3477Main")
        window.makeKeyAndOrderFront(nil)

        // the page sets its own <title>; mirror it in the window title
        titleObservation = webView.observe(\.title, options: [.new]) { [weak self] wv, _ in
            if let t = wv.title, !t.isEmpty { self?.window.title = t }
        }
        NSApp.activate(ignoringOtherApps: true)
    }

    func load(_ url: URL) {
        serverHost = url.host.map { h in url.port.map { "\(h):\($0)" } ?? h } ?? ""
        webView.load(URLRequest(url: url))
        window.makeFirstResponder(webView)
    }

    /// Shows a startup/runtime failure of the server and quits.
    func fail(_ msg: String) {
        let a = NSAlert()
        a.alertStyle = .critical
        a.messageText = "The IBM 3477 terminal stopped"
        a.informativeText = msg
        a.runModal()
        NSApp.terminate(nil)
    }

    // MARK: navigation

    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if action.shouldPerformDownload {
            decisionHandler(.download) // <a download> of a print job
            return
        }
        // a link off the terminal's own server opens in the user's browser
        if action.navigationType == .linkActivated, let u = action.request.url,
           let h = u.host, (u.port.map { "\(h):\($0)" } ?? h) != serverHost,
           u.scheme == "http" || u.scheme == "https" {
            NSWorkspace.shared.open(u)
            decisionHandler(.cancel)
            return
        }
        decisionHandler(.allow)
    }

    func webView(_ webView: WKWebView, decidePolicyFor response: WKNavigationResponse,
                 decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        decisionHandler(response.canShowMIMEType ? .allow : .download)
    }

    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        download.delegate = self
    }

    /// The web content process can die (memory pressure); reload the page
    /// instead of leaving a blank window. The terminal reconnects.
    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        webView.reload()
    }

    // MARK: downloads (print jobs saved as PDF or text)

    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse,
                  suggestedFilename: String, completionHandler: @escaping (URL?) -> Void) {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = suggestedFilename
        panel.directoryURL = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first
        panel.canCreateDirectories = true
        panel.beginSheetModal(for: window) { resp in
            guard resp == .OK, let url = panel.url else {
                completionHandler(nil) // cancelled: the download is dropped
                return
            }
            // WKDownload refuses to overwrite; the panel already asked
            try? FileManager.default.removeItem(at: url)
            completionHandler(url)
        }
    }

    func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
        let a = NSAlert(error: error)
        a.beginSheetModal(for: window)
    }

    // MARK: UI delegate

    /// window.open: no second window; the page then downloads the PDF.
    func webView(_ webView: WKWebView, createWebViewWith cfg: WKWebViewConfiguration,
                 for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        nil
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.beginSheetModal(for: window) { _ in completionHandler() }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.addButton(withTitle: "OK")
        a.addButton(withTitle: "Cancel")
        a.beginSheetModal(for: window) { r in completionHandler(r == .alertFirstButtonReturn) }
    }

    // MARK: menus

    /// The standard app, Edit, View and Window menus. Edit matters: without
    /// its Paste item Cmd+V never reaches the page's paste field.
    func buildMenu() {
        let main = NSMenu()
        let name = "IBM 3477"

        let app = NSMenu()
        app.addItem(withTitle: "About \(name)",
                    action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        app.addItem(.separator())
        app.addItem(withTitle: "Hide \(name)", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        let others = app.addItem(withTitle: "Hide Others",
                                 action: #selector(NSApplication.hideOtherApplications(_:)), keyEquivalent: "h")
        others.keyEquivalentModifierMask = [.command, .option]
        app.addItem(withTitle: "Show All", action: #selector(NSApplication.unhideAllApplications(_:)),
                    keyEquivalent: "")
        app.addItem(.separator())
        app.addItem(withTitle: "Quit \(name)", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        main.addItem(submenu(app, ""))

        let edit = NSMenu(title: "Edit")
        edit.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
        let redo = edit.addItem(withTitle: "Redo", action: Selector(("redo:")), keyEquivalent: "z")
        redo.keyEquivalentModifierMask = [.command, .shift]
        edit.addItem(.separator())
        edit.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        main.addItem(submenu(edit, "Edit"))

        let view = NSMenu(title: "View")
        view.addItem(withTitle: "Reload Page", action: #selector(reloadPage(_:)), keyEquivalent: "r")
        let fs = view.addItem(withTitle: "Enter Full Screen",
                              action: #selector(NSWindow.toggleFullScreen(_:)), keyEquivalent: "f")
        fs.keyEquivalentModifierMask = [.command, .control]
        main.addItem(submenu(view, "View"))

        let win = NSMenu(title: "Window")
        win.addItem(withTitle: "Minimize", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        win.addItem(withTitle: "Zoom", action: #selector(NSWindow.performZoom(_:)), keyEquivalent: "")
        win.addItem(withTitle: "Close", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        main.addItem(submenu(win, "Window"))
        NSApp.windowsMenu = win

        NSApp.mainMenu = main
    }

    private func submenu(_ m: NSMenu, _ title: String) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        item.submenu = m
        return item
    }

    @objc func reloadPage(_ sender: Any?) { webView.reload() }
}

// MARK: - main

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
