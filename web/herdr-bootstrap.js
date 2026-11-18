const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.0-508-6ef1592a7b464d9f/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
