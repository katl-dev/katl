Name:           crun
Version:        %{upstream_version}
Release:        1.katl%{?dist}
Summary:        KatlOS OCI runtime with systemd cgroups and seccomp
Vendor:         KatlOS
License:        GPL-2.0-or-later AND LGPL-2.1-or-later
URL:            https://github.com/containers/crun
Source0:        crun-%{version}.tar.zst
Source1:        crun-source.json
BuildRequires:  gcc, make, pkgconf-pkg-config, python3, binutils
BuildRequires:  libcap-devel, libseccomp-devel, systemd-devel, json-c-devel
Provides:       oci-runtime

%description
Crun built for KatlOS against Fedora host libraries, with systemd cgroups,
capabilities, and seccomp support.

%prep
%setup -q
cp %{SOURCE1} katl-source.json

%build
%configure --enable-systemd --enable-seccomp --enable-caps --enable-embedded-blake3 \
    --disable-shared --without-python-bindings --without-lua-bindings
%make_build

%install
install -Dpm0755 crun %{buildroot}%{_bindir}/crun

%check
./crun --version | grep -F '%{version}'
./crun --version | grep -F '+SYSTEMD'
./crun --version | grep -F '+SECCOMP'
./crun --version | grep -F '+CAP'
readelf -h ./crun | grep -E 'Type:.*DYN'

%files
%license COPYING COPYING.libcrun
%doc katl-source.json
%{_bindir}/crun
