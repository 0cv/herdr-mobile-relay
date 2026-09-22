const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-387-b369518bfd47989a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
