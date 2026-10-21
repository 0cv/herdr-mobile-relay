const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.10-414-42e6a68964889aee/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
