const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-405-0f9163a5637d8618/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
