%{!?neu_box_version:%global neu_box_version 0.0.0}
%{!?neu_box_release:%global neu_box_release 1}

Name:           neuboxd
Version:        %{neu_box_version}
Release:        %{neu_box_release}
Summary:        Neu Box Worker, client, and OCI runtime
License:        MIT
URL:            https://github.com/neusbox/neu_box
Source0:        %{name}-%{version}-%{release}.tar.gz

ExclusiveArch:  x86_64 aarch64
Requires:       /bin/bash
Requires:       systemd

# PyInstaller resolves libraries below _internal itself. Do not advertise those
# private copies as system capabilities or turn their internal edges into host
# dependencies. Keep automatic discovery enabled for the top-level bootloaders,
# native sandbox, and the rest of the package.
%global __provides_exclude_from ^%{_libexecdir}/neu-box/(neuboxd|ctl|tests)/_internal/.*\\.so.*$
%global __requires_exclude_from ^%{_libexecdir}/neu-box/(neuboxd|ctl|tests)/_internal/.*$

# The worker and sandbox are already-built release artifacts.  Keep this RPM
# as their owner instead of rebuilding them in a package scriptlet.
%global debug_package %{nil}
# GNU strip on some distributions treats the eBPF ELF object as an unknown
# architecture. All payloads are already finalized, so do not mutate them.
%global __strip /bin/true

%description
Neu Box runs accelerator jobs and enforces their device isolation. This RPM
ships the Worker, neubox CLI, OCI runtime wrapper, hook, native
sandbox, BPF object, deployment tests, and systemd unit together.

%prep
%autosetup -p1

%build
# All executable payloads are built before rpmbuild.

%install
rm -rf %{buildroot}
cp -a rootfs/. %{buildroot}/

test -x %{buildroot}%{_libexecdir}/neu-box/neuboxd/neuboxd
test -x %{buildroot}/usr/local/bin/neubox
test -x %{buildroot}%{_libexecdir}/neu-box/neu-box-runtime
test -x %{buildroot}%{_libexecdir}/neu-box/neu-box-hook
test -x %{buildroot}%{_libexecdir}/neu-box/ctl/neuboxctl
test -L %{buildroot}%{_libexecdir}/neu-box/bin/neuboxctl
test "$(readlink %{buildroot}%{_libexecdir}/neu-box/bin/neuboxctl)" = ../ctl/neuboxctl
test -x %{buildroot}%{_libexecdir}/neu-box/neu-box-sandbox
test -f %{buildroot}%{_libexecdir}/neu-box/device_block.o
test -x %{buildroot}%{_libexecdir}/neu-box/tests/neu-box-deployment-tests
test ! -e %{buildroot}%{_sbindir}/neuboxctl
test ! -e %{buildroot}%{_sbindir}/neuboxd
test ! -e %{buildroot}/usr/local/bin/neu-box-runtime
test ! -e %{buildroot}/usr/local/bin/neu-box-hook
test -f %{buildroot}%{_unitdir}/neuboxd.service
test -f %{buildroot}%{_sysconfdir}/neu-box/worker.env
test -f %{buildroot}%{_datadir}/neu-box/runtime.env.example
test ! -e %{buildroot}%{_sysconfdir}/neu-box/runtime.env

%pre
# Replacing an onedir PyInstaller bundle below a live process is unsafe: it can
# import another bundled module after RPM has changed the files.
if /usr/bin/systemctl is-active --quiet \
    neuboxd.service >/dev/null 2>&1; then
    ctl=/usr/libexec/neu-box/bin/neuboxctl
    if [ ! -x "$ctl" ]; then
        ctl=/usr/libexec/neu-box/neuboxctl/neuboxctl
    fi
    echo "neuboxd.service 正在运行；安装前请执行 '$ctl pause'" >&2
    exit 1
fi

%post
# Deliberately do not enable or start the service. Database/config migration
# belongs to the deployment workflow, not an RPM scriptlet.
/usr/bin/systemctl daemon-reload >/dev/null 2>&1 || :

%posttrans
# DNF may print verification and transaction summaries after this hook.
cat <<'EOF'

================================================================
 Neu Box 安装完成
----------------------------------------------------------------
 配置  /etc/neu-box/worker.env（升级时保留现有配置）
 启用  sudo /usr/libexec/neu-box/bin/neuboxctl setup
 验收  sudo /usr/libexec/neu-box/bin/neuboxctl test
 文档  rpm -qd neuboxd
================================================================
EOF

%preun
if [ "$1" -eq 0 ]; then
    if [ -f /etc/docker/daemon.json ] && \
        grep -q '"neu-box-runtime"' /etc/docker/daemon.json 2>/dev/null; then
        echo "Docker 配置仍引用 neu-box-runtime；卸载前请先移除该配置" >&2
        exit 1
    fi
    if /usr/bin/systemctl is-active --quiet \
        neuboxd.service >/dev/null 2>&1; then
        echo "neuboxd.service 正在运行；卸载前请执行 '/usr/libexec/neu-box/bin/neuboxctl pause'" >&2
        exit 1
    fi
    /usr/bin/systemctl disable neuboxd.service >/dev/null 2>&1 || :
fi

%postun
if [ -x /usr/bin/systemctl ]; then
    /usr/bin/systemctl daemon-reload >/dev/null 2>&1 || :
fi

%files
%license LICENSE
%doc DEPLOYMENT.md
%attr(0755,root,root) /usr/local/bin/neubox
%dir %{_libexecdir}/neu-box
%attr(0755,root,root) %{_libexecdir}/neu-box/neu-box-runtime
%attr(0755,root,root) %{_libexecdir}/neu-box/neu-box-hook
%{_libexecdir}/neu-box/neuboxd
%{_libexecdir}/neu-box/ctl
%dir %{_libexecdir}/neu-box/bin
%{_libexecdir}/neu-box/bin/neuboxctl
%attr(0755,root,root) %{_libexecdir}/neu-box/neu-box-sandbox
%attr(0644,root,root) %{_libexecdir}/neu-box/device_block.o
%dir %{_libexecdir}/neu-box/tests
%{_libexecdir}/neu-box/tests/*
%dir %{_datadir}/neu-box
%{_datadir}/neu-box/info
%attr(0644,root,root) %{_datadir}/neu-box/runtime.env.example
%attr(0644,root,root) %{_datadir}/neu-box/manifest.json
%attr(0644,root,root) %{_unitdir}/neuboxd.service
%config(noreplace) %attr(0640,root,root) %{_sysconfdir}/neu-box/worker.env
%dir %attr(0750,root,root) %{_localstatedir}/lib/neu-box/worker

%changelog
* Tue Sep 01 2026 Neu Box contributors <noreply@neu-box.local> - %{neu_box_version}-%{neu_box_release}
- Initial RPM packaging
