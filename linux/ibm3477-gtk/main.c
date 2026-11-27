/*
 * ibm3477 — native Linux app for the emulated IBM InfoWindow terminal.
 *
 * The Linux twin of macos/IBM3477/main.swift. The emulator and its web
 * page stay in Go (cmd/appwebibm3477); this program replaces the browser
 * tab with a GTK window holding a WebKitGTK web view:
 *
 *   ibm3477          this program (GTK 3 + WebKitGTK 4.1)
 *   appwebibm3477    the Go server, started as  appwebibm3477 -embedded
 *
 * The server prints "APPURL <url>" on stdout once it serves; we load that
 * URL. Its lifetime is tied to ours twice over:
 *   - its stdin is a pipe whose write end only we hold: when we end in any
 *     way the kernel closes it, the server sees EOF and quits (embedded.go);
 *   - PR_SET_PDEATHSIG sends it SIGTERM when we die, as a second guard.
 *
 * What a browser did for the page and a bare web view does not is done
 * here: print jobs the page downloads (PDF/text) go through a GTK Save
 * dialog, window.open is refused (the page then downloads the PDF), and
 * links to other sites open in the user's default browser.
 *
 * Build: tools/make-linux-app.bash (in a container, from ./build.bash).
 */
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/wait.h>
#include <unistd.h>

#include <glib-unix.h>
#include <gtk/gtk.h>
#include <webkit2/webkit2.h>

#define SERVER_NAME "appwebibm3477"
#define URL_PREFIX "APPURL "         /* stdout line with the URL (embedded.go) */
#define STARTUP_TIMEOUT_S 30         /* time the server gets to print its URL */
#define ERR_KEEP 4096                /* bytes of server stderr kept for errors */

static GtkWidget *window;
static WebKitWebView *view;
static GPid server_pid;
static int server_stdin = -1;        /* our end; closing it stops the server */
static gboolean server_ready;
static gboolean stopping;            /* quitting: the server's exit is expected */
static GString *err_text;            /* tail of the server's stderr */
static char server_hostport[300];    /* "127.0.0.1:53478": links elsewhere open outside */

/* ---- errors ----------------------------------------------------------- */

/* fail shows why the terminal stopped and ends the app. */
static void fail(const char *msg)
{
	if (stopping)
		return;
	stopping = TRUE;
	GtkWidget *d = gtk_message_dialog_new(window ? GTK_WINDOW(window) : NULL,
		GTK_DIALOG_MODAL, GTK_MESSAGE_ERROR, GTK_BUTTONS_CLOSE,
		"The IBM 3477 terminal stopped");
	gtk_message_dialog_format_secondary_text(GTK_MESSAGE_DIALOG(d), "%s", msg);
	gtk_dialog_run(GTK_DIALOG(d));
	gtk_widget_destroy(d);
	gtk_main_quit();
}

/* ---- server process --------------------------------------------------- */

/* find_server returns the server binary: next to this program (the
 * tarball and install layout), else on $PATH. Caller frees. */
static char *find_server(void)
{
	char self[4096];
	ssize_t n = readlink("/proc/self/exe", self, sizeof self - 1);
	if (n > 0) {
		self[n] = 0;
		char *dir = g_path_get_dirname(self);
		char *p = g_build_filename(dir, SERVER_NAME, NULL);
		g_free(dir);
		if (g_file_test(p, G_FILE_TEST_IS_EXECUTABLE))
			return p;
		g_free(p);
	}
	return g_find_program_in_path(SERVER_NAME);
}

/* Runs in the child between fork and exec: SIGTERM it when we die. */
static void child_setup(gpointer unused)
{
	(void)unused;
	prctl(PR_SET_PDEATHSIG, SIGTERM);
}

/* set_host remembers host:port of the server URL (for the link check). */
static void set_host(const char *url)
{
	const char *h = strstr(url, "://");
	h = h ? h + 3 : url;
	size_t len = strcspn(h, "/?#");
	if (len >= sizeof server_hostport)
		len = sizeof server_hostport - 1;
	memcpy(server_hostport, h, len);
	server_hostport[len] = 0;
}

/* on_stdout reads the server's stdout line by line. The first APPURL line
 * is loaded; everything else is drained and ignored, so the server never
 * blocks on a full pipe.
 * The channel is non-blocking (start_server): a blocking read here would
 * freeze the whole GTK main loop. It reads every complete line now in the
 * channel's buffer, because the watch fires again only for new data on
 * the pipe, not for lines already buffered. */
static gboolean on_stdout(GIOChannel *ch, GIOCondition cond, gpointer unused)
{
	(void)cond; (void)unused;
	GIOStatus st;
	for (;;) {
		char *line = NULL;
		gsize len = 0;
		st = g_io_channel_read_line(ch, &line, &len, NULL, NULL);
		if (st != G_IO_STATUS_NORMAL || !line) {
			g_free(line);
			break; /* AGAIN: no full line yet; EOF/ERROR: server gone */
		}
		g_strchomp(line);
		if (!server_ready && g_str_has_prefix(line, URL_PREFIX)) {
			const char *url = line + strlen(URL_PREFIX);
			server_ready = TRUE;
			set_host(url);
			webkit_web_view_load_uri(view, url);
		}
		g_free(line);
	}
	return st == G_IO_STATUS_AGAIN; /* keep watching until EOF */
}

/* on_stderr keeps the last ERR_KEEP bytes for error messages and passes
 * everything on to our own stderr (visible when started from a shell).
 * Plain read(): it returns what the pipe holds now, where GIOChannel's
 * read_chars would block until its whole buffer is full. */
static gboolean on_stderr(GIOChannel *ch, GIOCondition cond, gpointer unused)
{
	(void)cond; (void)unused;
	char buf[1024];
	ssize_t n = read(g_io_channel_unix_get_fd(ch), buf, sizeof buf);
	if (n < 0 && (errno == EINTR || errno == EAGAIN))
		return TRUE;
	if (n > 0) {
		if (write(STDERR_FILENO, buf, n) < 0) { /* nowhere to report */ }
		g_string_append_len(err_text, buf, n);
		if (err_text->len > ERR_KEEP)
			g_string_erase(err_text, 0, err_text->len - ERR_KEEP);
	}
	return n > 0; /* 0 = EOF, <0 = error: stop watching */
}

/* on_server_exit: the server ended. Expected while quitting; otherwise
 * show its last error output. */
static void on_server_exit(GPid pid, gint status, gpointer unused)
{
	(void)unused;
	g_spawn_close_pid(pid);
	server_pid = 0;
	if (stopping)
		return;
	char *msg = g_strstrip(g_strdup(err_text->str));
	if (!*msg) {
		g_free(msg);
		msg = g_strdup_printf("exit status %d", WEXITSTATUS(status));
	}
	fail(msg);
	g_free(msg);
}

static gboolean on_startup_timeout(gpointer unused)
{
	(void)unused;
	if (!server_ready)
		fail("The terminal server did not start within 30 s.");
	return G_SOURCE_REMOVE;
}

static void start_server(void)
{
	char *exe = find_server();
	if (!exe) {
		fail(SERVER_NAME " was not found next to this program or on $PATH.");
		return;
	}
	char *argv[] = { exe, "-embedded", NULL };
	int out = -1, errfd = -1;
	GError *err = NULL;
	if (!g_spawn_async_with_pipes(g_get_home_dir(), argv, NULL,
			G_SPAWN_DO_NOT_REAP_CHILD, child_setup, NULL,
			&server_pid, &server_stdin, &out, &errfd, &err)) {
		fail(err->message);
		g_error_free(err);
		g_free(exe);
		return;
	}
	g_free(exe);
	/* our end of stdin must not leak into programs we start later
	 * (the default browser), or the server would outlive us */
	fcntl(server_stdin, F_SETFD, FD_CLOEXEC);

	GIOChannel *o = g_io_channel_unix_new(out);
	g_io_channel_set_close_on_unref(o, TRUE);
	g_io_channel_set_flags(o, G_IO_FLAG_NONBLOCK, NULL); /* see on_stdout */
	g_io_add_watch(o, G_IO_IN | G_IO_HUP | G_IO_ERR, on_stdout, NULL);
	g_io_channel_unref(o);
	GIOChannel *e = g_io_channel_unix_new(errfd);
	g_io_channel_set_close_on_unref(e, TRUE);
	g_io_add_watch(e, G_IO_IN | G_IO_HUP | G_IO_ERR, on_stderr, NULL);
	g_io_channel_unref(e);

	g_child_watch_add(server_pid, on_server_exit, NULL);
	g_timeout_add_seconds(STARTUP_TIMEOUT_S, on_startup_timeout, NULL);
}

/* stop_server asks the server to quit: close its stdin and send SIGTERM. */
static void stop_server(void)
{
	stopping = TRUE;
	if (server_stdin >= 0) {
		close(server_stdin);
		server_stdin = -1;
	}
	if (server_pid > 0)
		kill(server_pid, SIGTERM);
}

/* ---- downloads (print jobs saved as PDF or text) ---------------------- */

/* on_decide_destination asks where to save, with a GTK Save dialog that
 * starts in the Downloads folder. Cancel drops the download. */
static gboolean on_decide_destination(WebKitDownload *dl, gchar *suggested,
                                      gpointer unused)
{
	(void)unused;
	GtkWidget *d = gtk_file_chooser_dialog_new("Save print job",
		GTK_WINDOW(window), GTK_FILE_CHOOSER_ACTION_SAVE,
		"_Cancel", GTK_RESPONSE_CANCEL, "_Save", GTK_RESPONSE_ACCEPT, NULL);
	GtkFileChooser *fc = GTK_FILE_CHOOSER(d);
	gtk_file_chooser_set_do_overwrite_confirmation(fc, TRUE);
	const char *dir = g_get_user_special_dir(G_USER_DIRECTORY_DOWNLOAD);
	if (dir)
		gtk_file_chooser_set_current_folder(fc, dir);
	gtk_file_chooser_set_current_name(fc, suggested);
	if (gtk_dialog_run(GTK_DIALOG(d)) == GTK_RESPONSE_ACCEPT) {
		char *uri = gtk_file_chooser_get_uri(fc);
		/* the dialog already asked about replacing the file */
		webkit_download_set_allow_overwrite(dl, TRUE);
		webkit_download_set_destination(dl, uri);
		g_free(uri);
	} else {
		webkit_download_cancel(dl);
	}
	gtk_widget_destroy(d);
	return TRUE;
}

static void on_download_failed(WebKitDownload *dl, GError *err, gpointer unused)
{
	(void)dl; (void)unused;
	if (g_error_matches(err, WEBKIT_DOWNLOAD_ERROR,
	                    WEBKIT_DOWNLOAD_ERROR_CANCELLED_BY_USER))
		return; /* Cancel in the Save dialog */
	GtkWidget *d = gtk_message_dialog_new(GTK_WINDOW(window), GTK_DIALOG_MODAL,
		GTK_MESSAGE_ERROR, GTK_BUTTONS_CLOSE, "Saving failed");
	gtk_message_dialog_format_secondary_text(GTK_MESSAGE_DIALOG(d), "%s",
	                                         err->message);
	gtk_dialog_run(GTK_DIALOG(d));
	gtk_widget_destroy(d);
}

static void on_download_started(WebKitWebContext *ctx, WebKitDownload *dl,
                                gpointer unused)
{
	(void)ctx; (void)unused;
	g_signal_connect(dl, "decide-destination", G_CALLBACK(on_decide_destination), NULL);
	g_signal_connect(dl, "failed", G_CALLBACK(on_download_failed), NULL);
}

/* ---- navigation ------------------------------------------------------- */

/* uri_hostport writes host[:port] of uri into out ("" if none). */
static void uri_hostport(const char *uri, char *out, size_t size)
{
	out[0] = 0;
	GUri *u = g_uri_parse(uri, G_URI_FLAGS_NONE, NULL);
	if (!u)
		return;
	const char *h = g_uri_get_host(u);
	int port = g_uri_get_port(u);
	if (h && port > 0)
		g_snprintf(out, size, "%s:%d", h, port);
	else if (h)
		g_snprintf(out, size, "%s", h);
	g_uri_unref(u);
}

static gboolean on_decide_policy(WebKitWebView *v, WebKitPolicyDecision *dec,
                                 WebKitPolicyDecisionType type, gpointer unused)
{
	(void)v; (void)unused;
	if (type == WEBKIT_POLICY_DECISION_TYPE_RESPONSE) {
		/* what the view cannot show (a PDF) is a download */
		WebKitResponsePolicyDecision *r = WEBKIT_RESPONSE_POLICY_DECISION(dec);
		if (!webkit_response_policy_decision_is_mime_type_supported(r)) {
			webkit_policy_decision_download(dec);
			return TRUE;
		}
		return FALSE;
	}
	if (type != WEBKIT_POLICY_DECISION_TYPE_NAVIGATION_ACTION)
		return FALSE; /* new windows are refused in on_create */
	WebKitNavigationAction *a = webkit_navigation_policy_decision_get_navigation_action(
		WEBKIT_NAVIGATION_POLICY_DECISION(dec));
	if (webkit_navigation_action_get_navigation_type(a) != WEBKIT_NAVIGATION_TYPE_LINK_CLICKED)
		return FALSE;
	/* a clicked link off the terminal's own server opens in the user's
	 * browser instead of replacing the terminal */
	const char *uri = webkit_uri_request_get_uri(webkit_navigation_action_get_request(a));
	char hp[300];
	uri_hostport(uri, hp, sizeof hp);
	if ((g_str_has_prefix(uri, "http://") || g_str_has_prefix(uri, "https://")) &&
	    strcmp(hp, server_hostport) != 0) {
		g_app_info_launch_default_for_uri(uri, NULL, NULL);
		webkit_policy_decision_ignore(dec);
		return TRUE;
	}
	return FALSE;
}

/* window.open: no second window; the page then downloads the PDF. */
static GtkWidget *on_create(WebKitWebView *v, WebKitNavigationAction *a, gpointer unused)
{
	(void)v; (void)a; (void)unused;
	return NULL;
}

/* The web process can die (out of memory); reload instead of leaving an
 * empty window. The terminal page reconnects by itself. */
static void on_web_process_terminated(WebKitWebView *v,
                                      WebKitWebProcessTerminationReason r, gpointer unused)
{
	(void)r; (void)unused;
	webkit_web_view_reload(v);
}

static void on_load_changed(WebKitWebView *v, WebKitLoadEvent ev, gpointer unused)
{
	(void)unused;
	/* one line for whoever started us from a shell (and the smoke test) */
	if (ev == WEBKIT_LOAD_FINISHED)
		g_printerr("ibm3477: page loaded from %s\n", server_hostport);
	(void)v;
}

/* the page sets its own <title>; mirror it in the window title */
static void on_title(WebKitWebView *v, GParamSpec *ps, gpointer unused)
{
	(void)ps; (void)unused;
	const char *t = webkit_web_view_get_title(v);
	if (t && *t)
		gtk_window_set_title(GTK_WINDOW(window), t);
}

/* ---- window size, remembered between runs ------------------------------ */

/* The size and maximized state are kept in ~/.config/ibm3477/window.ini
 * (GKeyFile). They are tracked while the window lives and written once at
 * quit; the position is left to the window manager (Wayland has none). */
static int win_w = 1280, win_h = 820;
static gboolean win_max;

static char *window_ini(void)
{
	return g_build_filename(g_get_user_config_dir(), "ibm3477", "window.ini", NULL);
}

static void load_window_state(void)
{
	char *path = window_ini();
	GKeyFile *kf = g_key_file_new();
	if (g_key_file_load_from_file(kf, path, G_KEY_FILE_NONE, NULL)) {
		int w = g_key_file_get_integer(kf, "window", "width", NULL);
		int h = g_key_file_get_integer(kf, "window", "height", NULL);
		if (w >= 640 && h >= 400) { /* ignore missing or silly values */
			win_w = w;
			win_h = h;
		}
		win_max = g_key_file_get_boolean(kf, "window", "maximized", NULL);
	}
	g_key_file_free(kf);
	g_free(path);
}

static void save_window_state(void)
{
	char *path = window_ini();
	char *dir = g_path_get_dirname(path);
	g_mkdir_with_parents(dir, 0700);
	GKeyFile *kf = g_key_file_new();
	g_key_file_set_integer(kf, "window", "width", win_w);
	g_key_file_set_integer(kf, "window", "height", win_h);
	g_key_file_set_boolean(kf, "window", "maximized", win_max);
	g_key_file_save_to_file(kf, path, NULL);
	g_key_file_free(kf);
	g_free(dir);
	g_free(path);
}

/* size changes: remember the normal (unmaximized) size only, so that
 * un-maximizing after the next start returns to it */
static gboolean on_configure(GtkWidget *w, GdkEvent *ev, gpointer unused)
{
	(void)ev; (void)unused;
	if (!win_max)
		gtk_window_get_size(GTK_WINDOW(w), &win_w, &win_h);
	return FALSE;
}

static gboolean on_window_state(GtkWidget *w, GdkEventWindowState *ev, gpointer unused)
{
	(void)w; (void)unused;
	win_max = (ev->new_window_state & GDK_WINDOW_STATE_MAXIMIZED) != 0;
	return FALSE;
}

/* ---- main ------------------------------------------------------------- */

/* SIGTERM/SIGINT/SIGHUP (logout, Ctrl-C in a shell): quit cleanly. */
static gboolean on_signal(gpointer unused)
{
	(void)unused;
	gtk_main_quit();
	return G_SOURCE_REMOVE;
}

/* set_icon uses ibm3477.png next to the program, if there. */
static void set_icon(void)
{
	char self[4096];
	ssize_t n = readlink("/proc/self/exe", self, sizeof self - 1);
	if (n <= 0)
		return;
	self[n] = 0;
	char *dir = g_path_get_dirname(self);
	char *p = g_build_filename(dir, "ibm3477.png", NULL);
	if (g_file_test(p, G_FILE_TEST_EXISTS))
		gtk_window_set_default_icon_from_file(p, NULL);
	g_free(p);
	g_free(dir);
}

int main(int argc, char **argv)
{
	/* the name also picks WebKit's data folder (~/.local/share/ibm3477),
	 * where the page's settings (localStorage) are kept between runs,
	 * and matches StartupWMClass in ibm3477.desktop for the dock icon */
	g_set_prgname("ibm3477");
	g_set_application_name("IBM 3477");
	gtk_init(&argc, &argv);
	err_text = g_string_new(NULL);
	set_icon();

	WebKitUserContentManager *ucm = webkit_user_content_manager_new();
	/* mark the page as running in a native app before its CSS applies:
	 * style.css then sizes the toolbar for a window without browser
	 * chrome (html.native-app rules) and draws selects itself, since
	 * WebKitGTK's own are unreadable on this dark page (html.gtk-app) */
	webkit_user_content_manager_add_script(ucm, webkit_user_script_new(
		"document.documentElement.classList.add('native-app', 'gtk-app')",
		WEBKIT_USER_CONTENT_INJECT_TOP_FRAME,
		WEBKIT_USER_SCRIPT_INJECT_AT_DOCUMENT_START, NULL, NULL));
	view = WEBKIT_WEB_VIEW(webkit_web_view_new_with_user_content_manager(ucm));

	WebKitSettings *s = webkit_web_view_get_settings(view);
	/* the terminal clicks and beeps through Web Audio */
	webkit_settings_set_media_playback_requires_user_gesture(s, FALSE);
	/* copy (Ctrl+C on a selection) writes the clipboard from script */
	webkit_settings_set_javascript_can_access_clipboard(s, TRUE);
	/* Inspect Element in the context menu, for debugging */
	webkit_settings_set_enable_developer_extras(s, TRUE);

	g_signal_connect(view, "decide-policy", G_CALLBACK(on_decide_policy), NULL);
	g_signal_connect(view, "create", G_CALLBACK(on_create), NULL);
	g_signal_connect(view, "web-process-terminated", G_CALLBACK(on_web_process_terminated), NULL);
	g_signal_connect(view, "load-changed", G_CALLBACK(on_load_changed), NULL);
	g_signal_connect(view, "notify::title", G_CALLBACK(on_title), NULL);
	g_signal_connect(webkit_web_view_get_context(view), "download-started",
	                 G_CALLBACK(on_download_started), NULL);

	window = gtk_window_new(GTK_WINDOW_TOPLEVEL);
	gtk_window_set_title(GTK_WINDOW(window), "IBM 3477");
	load_window_state();
	gtk_window_set_default_size(GTK_WINDOW(window), win_w, win_h);
	if (win_max)
		gtk_window_maximize(GTK_WINDOW(window));
	g_signal_connect(window, "configure-event", G_CALLBACK(on_configure), NULL);
	g_signal_connect(window, "window-state-event", G_CALLBACK(on_window_state), NULL);
	gtk_container_add(GTK_CONTAINER(window), GTK_WIDGET(view));
	g_signal_connect(window, "destroy", G_CALLBACK(gtk_main_quit), NULL);
	gtk_widget_show_all(window);
	gtk_widget_grab_focus(GTK_WIDGET(view));

	g_unix_signal_add(SIGTERM, on_signal, NULL);
	g_unix_signal_add(SIGINT, on_signal, NULL);
	g_unix_signal_add(SIGHUP, on_signal, NULL);

	start_server();
	gtk_main();
	stop_server();
	save_window_state();
	return 0;
}
