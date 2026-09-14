const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.2-381-a718c0cac6f4b2f9/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
