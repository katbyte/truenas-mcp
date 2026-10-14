#!/usr/bin/env python3
"""
Writes down which values of the TrueNAS API are secrets: api-defs/truenas-secrets-<version>.json.

The description a NAS gives of its API (api-defs/truenas-methods-<version>.json) does not say which values are secrets. The TrueNAS source does: a
field annotated Secret[...] is one. This reads the source without running it, follows every method to the models of what it takes and answers, and
writes the place of each secret as a path into the method's arguments ("$[0].password") or its answer ("$[].provider", or "$" for an answer that
is a secret whole). Every path is then looked up in the NAS's own description, so a path this reading got wrong is reported, not written.

Usage:

    git clone --depth 1 --branch TS-25.10.7 --filter=blob:none --sparse https://github.com/truenas/middleware.git
    git -C middleware sparse-checkout set src/middlewared/middlewared/api src/middlewared/middlewared/plugins src/middlewared/middlewared/service
    .scripts/secrets-from-source.py middleware 25.10.7

The source's API version (api/v25_10_5) is the newest one in the checkout unless a third argument names another.
"""

import ast
import json
import pathlib
import re
import subprocess
import sys

LISTS = {"list", "List", "set", "Set", "frozenset", "tuple", "Tuple", "UniqueList", "Sequence", "Iterable"}
DICTS = {"dict", "Dict", "Mapping"}
NOTHING_INSIDE = {"Literal", "type", "Type", "Callable"}
SHAPE = {"type", "properties", "items", "anyOf", "oneOf", "allOf", "additionalProperties", "enum", "const", "$ref"}

# What the source does not mark and is a secret all the same, by the start of a method's name and the field's name. Nothing in the source says so:
# each was found by reading what a NAS describes for names that sound like a secret, then the source for what the field holds. They are written
# under "unmarked", apart from what the source marks, because a NAS does not hide them from anyone: api-defs/README.md has the account of each.
UNMARKED = [
    ("ssh.", ["privatekey", "host_key", "host_dsa_key", "host_ecdsa_key", "host_ed25519_key", "host_rsa_key"]),
    ("replication.", ["encryption_key"]),
    ("keychaincredential.generate_ssh_key_pair", ["private_key"]),
]


def name_of(node):
    """The plain name an expression refers to: Name, or the last part of a dotted one."""
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        return node.attr

    return None


def alias_of(field):
    """The name a field is sent under when that is not its own: Field(alias="pass"), as its default or inside Annotated."""
    calls = [field.value] if isinstance(field.value, ast.Call) else []
    calls += [n for n in ast.walk(field.annotation) if isinstance(n, ast.Call)]
    for call in calls:
        if name_of(call.func) != "Field":
            continue
        for k in call.keywords:
            if k.arg == "alias" and isinstance(k.value, ast.Constant):
                return k.value.value

    return None


class Models:
    """Every model class and type alias of one API version, as written, with the secrets each holds worked out on asking."""

    def __init__(self, root, version):
        self.classes = {}
        self.aliases = {}
        self.wrapped = {}
        for path in sorted((root / "api" / "base").rglob("*.py")) + sorted((root / "api" / version).glob("*.py")):
            self.read(path)

    def read(self, path):
        tree = ast.parse(path.read_text())
        for node in tree.body:
            if isinstance(node, ast.ClassDef):
                fields = {}
                for item in node.body:
                    if isinstance(item, ast.AnnAssign) and isinstance(item.target, ast.Name):
                        # a field is sent under its alias where it has one: pass_ in the source is "pass" to a caller
                        fields[alias_of(item) or item.target.id] = item.annotation
                self.classes[node.name] = {"bases": [name_of(b) for b in node.bases if name_of(b)], "fields": fields}

                for decorator in node.decorator_list:
                    if isinstance(decorator, ast.Call) and name_of(decorator.func) == "single_argument_args":
                        self.wrapped[node.name] = ("args", decorator.args[0].value)
                    if name_of(decorator) == "single_argument_result" or isinstance(decorator, ast.Call) and name_of(decorator.func) == "single_argument_result":
                        self.wrapped[node.name] = ("result", None)
            elif isinstance(node, ast.Assign) and len(node.targets) == 1 and isinstance(node.targets[0], ast.Name):
                self.aliases[node.targets[0].id] = node.value
            elif isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name) and node.value is not None:
                self.aliases[node.target.id] = node.value

    def fields(self, name, seen=None):
        """A class's fields with those of its bases, its own winning."""
        seen = seen or set()
        if name in seen or name not in self.classes:
            return {}
        seen.add(name)

        out = {}
        for base in self.classes[name]["bases"]:
            out.update(self.fields(base, seen))
        out.update(self.classes[name]["fields"])

        return out

    def secrets(self, node, visiting=()):
        """Where the secrets are in a value of this type: paths from the value, "" for the value itself."""
        if node is None:
            return set()

        if isinstance(node, ast.Constant):
            if isinstance(node.value, str):
                try:
                    return self.secrets(ast.parse(node.value, mode="eval").body, visiting)
                except SyntaxError:
                    return set()

            return set()

        if isinstance(node, ast.BinOp) and isinstance(node.op, ast.BitOr):
            return self.secrets(node.left, visiting) | self.secrets(node.right, visiting)

        if isinstance(node, ast.Subscript):
            outer = name_of(node.value)
            inner = node.slice.elts if isinstance(node.slice, ast.Tuple) else [node.slice]
            if outer == "Secret":
                return {""}
            if outer in NOTHING_INSIDE:
                return set()
            if outer == "Annotated":
                return self.secrets(inner[0], visiting)
            if outer in LISTS:
                return {"[]" + p for part in inner for p in self.secrets(part, visiting)}
            if outer in DICTS:
                return {".*" + p for p in self.secrets(inner[-1], visiting)}

            return {p for part in inner for p in self.secrets(part, visiting)}

        if isinstance(node, ast.Call):
            # query_result(Entry), single_argument_result(Model) and the like, met as an alias
            made = name_of(node.func)
            if made in ("query_result", "query_result_item", "get_instance_result", "config_result", "single_argument_result") and node.args:
                inside = self.secrets(node.args[0], visiting)
                if made == "query_result":
                    return inside | {"[]" + p for p in inside}

                return inside

            return set()

        name = name_of(node)
        if name is None or name in visiting:
            return set()
        if name in self.classes:
            return {"." + field + p for field, annotation in self.fields(name).items() for p in self.secrets(annotation, visiting + (name,))}
        if name in self.aliases:
            return self.secrets(self.aliases[name], visiting + (name,))

        return set()

    def named_for(self, method):
        """The models of a method's arguments and answer by their names alone, for a method no service declares in the usual way."""
        key = re.sub(r"[._]", "", method).lower()
        for name in self.classes:
            if name.endswith("Args") and name[:-4].lower() == key:
                answer = name[:-4] + "Result"

                return name, answer if answer in self.classes or answer in self.aliases else None

        return None, None

    def arguments(self, name):
        """Where the secrets are in a method's arguments, from the model of them: a field for each argument in order, or one argument of them all."""
        if name in self.wrapped and self.wrapped[name][0] == "args":
            return {"$[0]." + field + p for field, annotation in self.fields(name).items() for p in self.secrets(annotation, (name,))}

        return {f"$[{i}]" + p for i, annotation in enumerate(self.fields(name).values()) for p in self.secrets(annotation, (name,))}

    def answer(self, name):
        """Where the secrets are in a method's answer, from the model of it: its result field, or the model itself when it is the answer."""
        if name in self.wrapped and self.wrapped[name][0] == "result":
            return {"$." + field + p for field, annotation in self.fields(name).items() for p in self.secrets(annotation, (name,))}
        if name in self.aliases:
            return {"$" + p for p in self.secrets(self.aliases[name], (name,))}

        return {"$" + p for p in self.secrets(self.fields(name).get("result"), (name,))}

    def entries(self, name):
        """Where the secrets are in a list of a model, one of it or a count of it: what a query answers."""
        one = {"$" + p for p in self.secrets(ast.Name(id=name), ())}

        return one | {"$[]" + p[1:] for p in one}


def services(root, models):
    """Every method the source declares, with the secrets of its arguments and answer: {"pool.query": {"params": [...], "result": [...]}}."""
    methods = {}

    def keyword(call, name):
        for k in call.keywords:
            if k.arg == name:
                return k.value

        return None

    for path in sorted((root / "plugins").rglob("*.py")) + sorted((root / "service").glob("*.py")):
        try:
            tree = ast.parse(path.read_text())
        except SyntaxError:
            continue

        for cls in [n for n in ast.walk(tree) if isinstance(n, ast.ClassDef)]:
            namespace = cls.name.removesuffix("Service").lower()
            entry = None
            for item in cls.body:
                if isinstance(item, ast.ClassDef) and item.name == "Config":
                    for setting in item.body:
                        if isinstance(setting, ast.Assign) and isinstance(setting.targets[0], ast.Name):
                            if setting.targets[0].id == "namespace" and isinstance(setting.value, ast.Constant):
                                namespace = setting.value.value
                            if setting.targets[0].id == "entry":
                                entry = name_of(setting.value)

            if entry:
                # what a service of entries answers without declaring it: a list of them, one of them, or its one record of settings
                methods.setdefault(namespace + ".query", {"params": set(), "result": models.entries(entry)})
                for made in ("get_instance", "config"):
                    methods.setdefault(namespace + "." + made, {"params": set(), "result": {"$" + p for p in models.secrets(ast.Name(id=entry), ())}})

            for fn in cls.body:
                if not isinstance(fn, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    continue
                for decorator in fn.decorator_list:
                    if not isinstance(decorator, ast.Call):
                        continue

                    private = keyword(decorator, "private")
                    if isinstance(private, ast.Constant) and private.value:
                        continue

                    method = namespace + "." + fn.name.removeprefix("do_")
                    if name_of(decorator.func) == "api_method" and len(decorator.args) >= 2:
                        takes, answers = name_of(decorator.args[0]), name_of(decorator.args[1])
                        methods[method] = {"params": models.arguments(takes), "result": models.answer(answers)}
                    elif name_of(decorator.func) == "filterable_api_method":
                        item = name_of(keyword(decorator, "item"))
                        methods[method] = {"params": set(), "result": models.entries(item) if item else set()}

    return methods


def described(schema, path):
    """Whether the NAS's description of a value has anything at a path: the check that a path read from the source is a real one."""
    if path == "":
        return True
    if not isinstance(schema, dict):
        return False
    # a value described as anything at all, a title and no shape: how the description leaves a model inside the single row a query can answer
    if not SHAPE & schema.keys():
        return True

    for alternative in schema.get("anyOf", []) + schema.get("oneOf", []) + schema.get("allOf", []):
        if described(alternative, path):
            return True

    if path.startswith("[]"):
        items = schema.get("items")
        if isinstance(items, list):
            return any(described(i, path[2:]) for i in items)

        return items is not None and described(items, path[2:])

    if path.startswith(".*"):
        more = schema.get("additionalProperties")
        # a dictionary of anything is described as an object and no more
        return more is True or isinstance(more, dict) and described(more, path[2:]) or schema.get("type") == "object" and "properties" not in schema

    found = re.match(r"\.([A-Za-z0-9_]+)", path)
    if not found:
        return False
    properties = schema.get("properties", {})
    if found.group(1) in properties:
        return described(properties[found.group(1)], path[found.end():])

    return False


def named(schema, path, names):
    """The place of every field of one of these names in the NAS's description of a value, as paths."""
    if not isinstance(schema, dict):
        return set()

    places = set()
    for alternative in schema.get("anyOf", []) + schema.get("oneOf", []) + schema.get("allOf", []):
        places |= named(alternative, path, names)

    for name, inside in schema.get("properties", {}).items():
        if name in names:
            places.add(f"{path}.{name}")
        else:
            places |= named(inside, f"{path}.{name}", names)

    items = schema.get("items")
    for item in items if isinstance(items, list) else [items]:
        places |= named(item, path + "[]", names)

    return places


def under(path, marked):
    """Whether a path is one of the marked places or inside one."""
    return any(path == m or path.startswith(m + ".") or path.startswith(m + "[") for m in marked)


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)

    checkout, release = pathlib.Path(sys.argv[1]), sys.argv[2]
    root = checkout / "src" / "middlewared" / "middlewared"
    versions = sorted((p.name for p in (root / "api").glob("v*_*") if p.is_dir()), key=lambda v: [int(n) for n in re.findall(r"\d+", v)])
    version = sys.argv[3] if len(sys.argv) > 3 else versions[-1]

    here = pathlib.Path(__file__).resolve().parent.parent
    document = json.loads((here / "api-defs" / f"truenas-methods-{release}.json").read_text())

    models = Models(root, version)
    declared = services(root, models)

    out, unread, wrong = {}, [], []
    for method in sorted(document):
        if method not in declared:
            # a method added to a service from elsewhere, as each kind of cloud storage adds its own: its models are still named for it
            takes, answers = models.named_for(method)
            if takes is None:
                unread.append(method)

                continue
            declared[method] = {"params": models.arguments(takes), "result": models.answer(answers) if answers else set()}

        places = {"params": [], "result": []}
        for path in sorted(declared[method]["params"]):
            index = int(re.match(r"\$\[(\d+)\]", path).group(1))
            rest = path[path.index("]") + 1:]
            accepts = document[method]["accepts"]
            if index < len(accepts) and described(accepts[index], rest):
                places["params"].append(path)
            else:
                wrong.append(f"{method} params {path}")
        for path in sorted(declared[method]["result"]):
            returns = document[method]["returns"]
            if returns and described(returns[0], path[1:]):
                places["result"].append(path)
            else:
                wrong.append(f"{method} result {path}")

        if places["params"] or places["result"]:
            out[method] = {k: v for k, v in places.items() if v}

    # what the source leaves unmarked is looked for in the description itself, so that a field of the same name on a method added later is found
    unmarked = {}
    for prefix, names in UNMARKED:
        found = 0
        for method in sorted(document):
            if not method.startswith(prefix):
                continue

            marked = out.get(method, {})
            places = {"params": set(), "result": set()}
            for index, accepts in enumerate(document[method]["accepts"]):
                places["params"] |= named(accepts, f"$[{index}]", names)
            for returns in document[method]["returns"][:1]:
                places["result"] |= named(returns, "$", names)

            for side in places:
                kept = sorted(p for p in places[side] if not under(p, marked.get(side, [])))
                if kept:
                    unmarked.setdefault(method, {}).setdefault(side, [])
                    unmarked[method][side] = sorted(set(unmarked[method][side]) | set(kept))
                    found += len(kept)

        if not found:
            wrong.append(f"{prefix}* unmarked {', '.join(names)}")

    commit = subprocess.run(["git", "-C", str(checkout), "rev-parse", "HEAD"], capture_output=True, text=True, check=False).stdout.strip()
    result = {
        "source": {"repository": "https://github.com/truenas/middleware", "tag": "TS-" + release, "commit": commit, "api": version},
        "methods": out,
        "unmarked": unmarked,
        "not_read": unread,
    }
    target = here / "api-defs" / f"truenas-secrets-{release}.json"
    target.write_text(json.dumps(result, indent=2) + "\n")

    places = sum(len(v) for m in out.values() for v in m.values())
    print(f"{target.name}: {places} secret places in {len(out)} of the {len(document)} methods the NAS describes (source {version})")
    print(f"secrets the source does not mark: {sum(len(v) for m in unmarked.values() for v in m.values())} places in {len(unmarked)} methods")
    print(f"methods the description has that this could not follow to their models: {len(unread)}")
    for method in unread:
        print("  " + method)
    print(f"places the source marks that the description has nothing at: {len(wrong)}")
    for place in wrong:
        print("  " + place)

    if unread or wrong:
        sys.exit(1)


if __name__ == "__main__":
    main()
