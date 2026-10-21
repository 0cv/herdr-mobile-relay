const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.10-412-b58ec6ef01e22331/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
