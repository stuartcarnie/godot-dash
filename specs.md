# Dash Docset Specs

## Defining the TOC

Links are placed in the `<head>` section, and are order dependent. The format of a link is:

```html
<link href="//dash_ref[_some id]/[type]/[name]/isSection (1 or 0)">
```

where:

* `href` is the URL to the page

And then anchors are placed appropriately in the `<body>` section. The format of an anchor is:

```html
<a class="dashAnchor" name="//dash_ref[_some id]/[type]/[name]/isSection"></a>
```
