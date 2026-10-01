const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-397-097282421cc86700/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
