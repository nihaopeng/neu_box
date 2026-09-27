%{!?neu_box_version:%global neu_box_version 0.0.0}
%{!?neu_box_release:%global neu_box_release 1}

Name:           neu-box-runtime
Version:        %{neu_box_version}
Release:        %{neu_box_release}
Summary:        Neu Box OCI runtime wrapper and registration hook
License:        MIT
URL:            https://github.com/neusbox/neu_box
Source0:        %{name}-%{version}-%{release}.tar.gz

ExclusiveArch:  x86_64 aarch64

# /usr/local/bin is not the usual place for a packaged file, and it is the right
# one here: the path is fixed by the cross-repo contract (docs/runtime-hook.md,
# this repo: the daemon.json entry, the hook path the wrapper injects, and the
# built-in defaults of both binaries are all /usr/local/bin), and
# scripts/install.sh installs to the same place.  A copy under /usr/bin would be
# a second runtime on the box: daemon.json would name one of them and the next
# dnf upgrade would update the other.
%global neu_box_bindir /usr/local/bin

# Both payloads are prebuilt and checked by deploy/rpm/build_rpm.sh
# (CGO_ENABLED=0, -trimpath, -s -w).  This package owns them rather than
# rebuilding or rewriting them: a scriptlet has no toolchain, and re-stripping
# would only make the installed bytes differ from the artifact the build script
# verified.
%global debug_package %{nil}
%global __strip /bin/true

# No Requires for the payload.  All three binaries are static (CGO_ENABLED=0), so
# there is nothing to declare for them; rpmbuild still adds the automatic
# Requires(preun)/Requires(post) on /bin/sh for the scriptlets, which is fine --
# every openEuler box has it.  The runtime the wrapper
# forwards to (NEU_BOX_REAL_RUNC) and Docker itself are dependencies this package
# cannot name: on the reference hosts Docker installs runc as a plain file that
# belongs to no package at all, so `Requires: runc` would be unsatisfiable and
# `Requires: docker` would name a package that does not exist.  The check that
# matters -- never point daemon.json at a runtime that is not installed and
# executable -- belongs to the deployment step, and scripts/install.sh makes it.

%description
Neu Box Runtime puts containers under neu-box sandbox authorization.

It ships three binaries.  neu-box-runtime is a runc wrapper that holds the
Docker default-runtime slot, injects a registration hook into the OCI bundle of
annotated containers, and forwards every other argv to the real runtime
untouched.  neu-box-hook is that OCI hook: runc calls it before the container
payload starts, and it registers the container with the Neu Box Worker.
neu-box-config generates and migrates /etc/neu-box/runtime.env; the package
does not ship that file, because a config with two writers (a package template
and a deployment script) is a config nobody owns.

The package installs files only.  Making neu-box-runtime the Docker default
runtime -- editing /etc/docker/daemon.json and restarting dockerd -- is a
deployment step for an administrator in a maintenance window, and lives in
scripts/install.sh.

%prep
%autosetup -p1

%build
# All payloads are built before rpmbuild by deploy/rpm/build_rpm.sh.

%install
rm -rf %{buildroot}
cp -a rootfs/. %{buildroot}/

test -x %{buildroot}%{neu_box_bindir}/neu-box-runtime
test -x %{buildroot}%{neu_box_bindir}/neu-box-hook
test -x %{buildroot}%{neu_box_bindir}/neu-box-config
test -x %{buildroot}%{_libexecdir}/neu-box/runtime/scripts/install.sh
test -x %{buildroot}%{_libexecdir}/neu-box/runtime/scripts/uninstall.sh
test ! -e %{buildroot}%{_sysconfdir}/neu-box/runtime.env

%pre
# Nothing is refused here, and that is a decision rather than an omission.
#
# The worker RPM refuses to install while its service is active because a live
# PyInstaller onedir bundle can import another bundled module after RPM has
# already replaced files underneath it.  This package has no service and no such
# bundle: the payload is three self-contained static binaries, RPM replaces each
# file atomically, and containers already running keep the inodes they started
# with.
#
# The one hazard worth refusing -- swapping the binaries under a dockerd that
# can still spawn them -- cannot be detected from here, and refusing to install
# while dockerd runs would make this package uninstallable on every Docker host.

%post
# Deliberately does not touch /etc/docker/daemon.json and does not restart
# dockerd.  Registering neu-box as the default runtime is a deployment decision:
# the restart kills every container that is running at that moment, so it
# belongs to an administrator in a maintenance window.  scripts/install.sh does
# that part and does not restart dockerd either.
#
# The package also ships no runtime.env: that file is generated and migrated by
# neu-box-config, which scripts/install.sh calls.  One writer per file -- see
# the header of scripts/install.sh.
#
# There is no unit of ours to daemon-reload -- the worker RPM has that line
# because it ships a service; this package ships three binaries and no config.
# Nothing is enabled, started, or migrated here.
if [ "$1" -eq 1 ]; then
    cat <<'EOF'
neu-box-runtime installed (files only) -- it is NOT active yet.
/etc/docker/daemon.json does not point at it, and dockerd has not been restarted.

After installing the matching Worker and runtime RPMs, deploy it with:

    sudo bash /usr/libexec/neu-box/runtime/scripts/install.sh --configure-only

then restart dockerd in a maintenance window: runtimes/default-runtime are not
hot-reloaded, and the restart kills every container running at that moment.
EOF
elif [ "$1" -ge 2 ]; then
    cat <<'EOF'
neu-box-runtime upgraded (files only).  Deploy it the same way as a fresh
install -- one command does the config migration and the daemon.json check.
On the deployed host:

    sudo bash /usr/libexec/neu-box/runtime/scripts/install.sh --configure-only
EOF
fi

%preun
# Erasing this package deletes %{neu_box_bindir}/neu-box-runtime.  While
# daemon.json still names neu-box-runtime that leaves dockerd unable to create any
# container at all -- the exact state scripts/install.sh orders itself to avoid
# (install binaries and verify first, edit daemon.json last) and
# scripts/uninstall.sh guards by restoring daemon.json before touching files.
#
# Only on final erase, and only a read: this never edits daemon.json.
if [ "$1" -eq 0 ] && [ -f %{_sysconfdir}/docker/daemon.json ]; then
    if grep -q '"neu-box-runtime"' %{_sysconfdir}/docker/daemon.json 2>/dev/null; then
        echo "daemon.json still names neu-box-runtime; restore it first" >&2
        echo "(sudo bash scripts/uninstall.sh, or remove the two keys by hand)" >&2
        exit 1
    fi
fi

%files
%dir %attr(0750,root,root) %{_sysconfdir}/neu-box
%attr(0755,root,root) %{neu_box_bindir}/neu-box-runtime
%attr(0755,root,root) %{neu_box_bindir}/neu-box-hook
%attr(0755,root,root) %{neu_box_bindir}/neu-box-config
%dir %{_libexecdir}/neu-box/runtime
%dir %{_libexecdir}/neu-box/runtime/scripts
%attr(0755,root,root) %{_libexecdir}/neu-box/runtime/scripts/install.sh
%attr(0755,root,root) %{_libexecdir}/neu-box/runtime/scripts/uninstall.sh

%changelog
* Sun Sep 13 2026 Neu Box contributors <noreply@neu-box.local> - %{neu_box_version}-%{neu_box_release}
- Initial RPM packaging
