const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.24.0-486-b7fa569f7418ee0d/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
