use crate::api::jsonschema;
use crate::api::schema::{Body, Cookie, Header, Method, Path, Query};
use crate::encore::parser::meta::v1 as meta;
use crate::encore::parser::meta::v1::path_segment::SegmentType;
use crate::encore::parser::schema::v1 as schema;
use crate::encore::parser::schema::v1::r#type::Typ;
use anyhow::Context;
use std::borrow::Cow;
use std::collections::{HashMap, HashSet};
use std::sync::Arc;

#[derive(Debug, Clone, Copy, PartialOrd, Ord, PartialEq, Eq, Hash)]
pub enum DefaultLoc {
    Body,
    Query,
}

impl DefaultLoc {
    pub fn into_wire_loc(self) -> WireLoc {
        match self {
            DefaultLoc::Body => WireLoc::Body,
            DefaultLoc::Query => WireLoc::Query,
        }
    }
}

#[derive(Debug, Clone, PartialOrd, Ord, PartialEq, Eq, Hash)]
pub enum WireLoc {
    Body,
    Query,
    Header(String),
    Path,
    Cookie(String),
}

pub struct EncodingConfig<'a, 'b> {
    pub meta: &'a meta::Data,
    pub registry_builder: &'a mut jsonschema::Builder<'b>,
    pub default_loc: Option<DefaultLoc>,
    pub rpc_path: Option<&'a meta::Path>,
    pub supports_body: bool,
    pub supports_query: bool,
    pub supports_header: bool,
    pub supports_path: bool,
}

#[derive(Debug)]
pub struct SchemaUnderConstruction {
    combined: Option<usize>,
    body: Option<usize>,
    query: Option<usize>,
    header: Option<usize>,
    cookie: Option<usize>,
    rpc_path: Option<meta::Path>,
}

impl SchemaUnderConstruction {
    pub fn build(self, reg: &Arc<jsonschema::Registry>) -> anyhow::Result<Schema> {
        Ok(Schema {
            combined: self.combined.map(|v| reg.schema(v)),
            body: self.body.map(|v| Body::new(reg.schema(v))),
            query: self.query.map(|v| Query::new(reg.schema(v))),
            header: self.header.map(|v| Header::new(reg.schema(v))),
            cookie: self.cookie.map(|v| Cookie::new(reg.schema(v))),
            path: self.rpc_path.as_ref().map(Path::from_meta).transpose()?,
        })
    }
}

#[derive(Debug, Clone)]
pub struct Schema {
    pub combined: Option<jsonschema::JSONSchema>,
    pub query: Option<Query>,
    pub header: Option<Header>,
    pub body: Option<Body>,
    pub path: Option<Path>,
    pub cookie: Option<Cookie>,
}

impl EncodingConfig<'_, '_> {
    pub fn compute(&mut self, typ: &schema::Type) -> anyhow::Result<SchemaUnderConstruction> {
        let typ = typ.typ.as_ref().context("type without type")?;
        let typ = resolve_type(self.meta, typ)?;

        let Typ::Struct(st) = typ.as_ref() else {
            return Ok(SchemaUnderConstruction {
                combined: None,
                body: None,
                query: None,
                header: None,
                cookie: None,
                rpc_path: self.rpc_path.cloned(),
            });
        };

        // Determine which fields belong to the path, if any.
        let path_fields = {
            let mut path_fields = HashSet::new();
            if let Some(rpc_path) = self.rpc_path {
                for seg in &rpc_path.segments {
                    let typ = SegmentType::try_from(seg.r#type).context("invalid segment type")?;
                    match typ {
                        SegmentType::Literal => {}
                        SegmentType::Param | SegmentType::Wildcard | SegmentType::Fallback => {
                            path_fields.insert(seg.value.as_str());
                        }
                    }
                }
            }
            path_fields
        };

        let mut combined = jsonschema::Struct::default();
        let mut body: Option<jsonschema::Struct> = None;
        let mut query: Option<jsonschema::Struct> = None;
        let mut header: Option<jsonschema::Struct> = None;
        let mut cookie: Option<jsonschema::Struct> = None;

        for f in &st.fields {
            // If it's a path field, skip it. We handle it separately in Path::from_meta.
            if path_fields.contains(f.name.as_str()) {
                continue;
            }

            let (name, mut field) = self.registry_builder.struct_field(f)?;
            combined.fields.insert(name.to_owned(), field.clone());

            // Resolve which location the field should be in.
            let loc = f.wire.as_ref().and_then(|w| w.location.as_ref());
            let wire_loc = match loc {
                None => self
                    .default_loc
                    .with_context(|| format!("no location defined for field {}", f.name))?
                    .into_wire_loc(),
                Some(schema::wire_spec::Location::Header(hdr)) => {
                    WireLoc::Header(hdr.name.as_ref().unwrap_or(&f.name).clone())
                }
                Some(schema::wire_spec::Location::Query(_)) => WireLoc::Query,
                Some(schema::wire_spec::Location::Cookie(c)) => {
                    WireLoc::Cookie(c.name.as_ref().unwrap_or(&f.name).clone())
                }
            };

            // Add the field to the appropriate struct.
            let (dst, name_override) = match wire_loc {
                WireLoc::Body => (&mut body, None),
                WireLoc::Query => (&mut query, None),
                WireLoc::Header(s) => (&mut header, Some(s)),
                WireLoc::Path => unreachable!(),
                WireLoc::Cookie(s) => (&mut cookie, Some(s)),
            };
            field.name_override = name_override;

            match dst {
                Some(dst) => {
                    dst.fields.insert(name.to_owned(), field);
                }
                None => {
                    *dst = Some(jsonschema::Struct {
                        fields: {
                            let mut fields = HashMap::new();
                            fields.insert(name.to_owned(), field);
                            fields
                        },
                    });
                }
            }
        }

        let mut build = |s| {
            self.registry_builder
                .register_value(jsonschema::Value::Struct(s))
        };

        Ok(SchemaUnderConstruction {
            combined: Some(build(combined)),
            body: body.map(&mut build),
            query: query.map(&mut build),
            header: header.map(&mut build),
            cookie: cookie.map(&mut build),
            rpc_path: self.rpc_path.cloned(),
        })
    }

    #[allow(dead_code)]
    fn resolve_struct<'b>(
        &'b self,
        typ: &'b schema::Type,
    ) -> anyhow::Result<Cow<'b, schema::Struct>> {
        let typ = typ.typ.as_ref().context("type without type")?;
        match typ {
            Typ::Struct(s) => Ok(Cow::Borrowed(s)),
            Typ::Pointer(ptr) => {
                let base = ptr.base.as_ref().context("pointer without base")?;
                self.resolve_struct(base)
            }
            Typ::Named(named) => {
                let decl = &self.meta.decls[named.id as usize];
                let typ = decl.r#type.as_ref().context("decl without type")?;
                self.resolve_struct(typ)
            }
            _ => anyhow::bail!("expected struct, got {:?}", typ),
        }
    }
}

fn resolve_type<'a>(meta: &'a meta::Data, typ: &'a Typ) -> anyhow::Result<Cow<'a, Typ>> {
    let resolver = TypeArgResolver {
        meta,
        resolved_args: vec![],
        decls: vec![],
    };
    resolver.resolve(typ)
}

struct TypeArgResolver<'a> {
    meta: &'a meta::Data,
    resolved_args: Vec<Cow<'a, Typ>>,

    /// List of declarations being processed.
    /// Used to detect cycles.
    decls: Vec<u32>,
}

impl<'a> TypeArgResolver<'a> {
    fn resolve(&self, typ: &'a Typ) -> anyhow::Result<Cow<'a, Typ>> {
        match typ {
            Typ::Named(named) => {
                let decl = &self.meta.decls[named.id as usize];
                if self.decls.contains(&decl.id) {
                    // Return it unmodified.
                    return Ok(Cow::Borrowed(typ));
                }

                let args = self.resolve_types(&named.type_arguments)?;
                let nested = TypeArgResolver {
                    meta: self.meta,
                    resolved_args: args,
                    decls: {
                        let mut decls = self.decls.clone();
                        decls.push(decl.id);
                        decls
                    },
                };
                let typ = decl.r#type.as_ref().context("decl without type")?;
                let typ = typ.typ.as_ref().context("type without type")?;
                nested.resolve(typ)
            }

            Typ::Struct(strukt) => {
                let mut cows = Vec::with_capacity(strukt.fields.len());
                for field in &strukt.fields {
                    let t = field.typ.as_ref().context("field without type")?;
                    let typ = t.typ.as_ref().context("type without type")?;
                    let resolved = self.resolve(typ)?;
                    cows.push((resolved, t.validation.as_ref()));
                }

                let mut fields = Vec::with_capacity(strukt.fields.len());
                for (field, (typ, v)) in strukt.fields.iter().zip(cows) {
                    fields.push(schema::Field {
                        typ: Some(schema::Type {
                            typ: Some(typ.into_owned()),
                            validation: v.cloned(),
                        }),
                        ..field.clone()
                    });
                }
                Ok(Cow::Owned(Typ::Struct(schema::Struct { fields })))
            }

            Typ::Map(map) => {
                let key = map.key.as_ref().context("map without key")?;
                let key_typ = key.typ.as_ref().context("key without type")?;
                let value = map.value.as_ref().context("map without value")?;
                let val_typ = value.typ.as_ref().context("value without type")?;
                let key_typ = self.resolve(key_typ)?;
                let val_typ = self.resolve(val_typ)?;

                if matches!((&key_typ, &val_typ), (Cow::Borrowed(_), Cow::Borrowed(_))) {
                    Ok(Cow::Borrowed(typ))
                } else {
                    Ok(Cow::Owned(Typ::Map(Box::new(schema::Map {
                        key: Some(Box::new(schema::Type {
                            typ: Some(key_typ.into_owned()),
                            validation: key.validation.clone(),
                        })),
                        value: Some(Box::new(schema::Type {
                            typ: Some(val_typ.into_owned()),
                            validation: value.validation.clone(),
                        })),
                    }))))
                }
            }

            Typ::List(list) => {
                let elem = list.elem.as_ref().context("list without elem")?;
                let elem_typ = elem.typ.as_ref().context("elem without type")?;
                let elem_typ = self.resolve(elem_typ)?;
                if matches!(elem_typ, Cow::Borrowed(_)) {
                    Ok(Cow::Borrowed(typ))
                } else {
                    Ok(Cow::Owned(Typ::List(Box::new(schema::List {
                        elem: Some(Box::new(schema::Type {
                            typ: Some(elem_typ.into_owned()),
                            validation: elem.validation.clone(),
                        })),
                    }))))
                }
            }

            Typ::Union(union) => {
                let types = self.resolve_types(&union.types)?;
                let types = types
                    .into_iter()
                    .zip(&union.types)
                    .map(|(typ, t)| schema::Type {
                        typ: Some(typ.into_owned()),
                        validation: t.validation.clone(),
                    })
                    .collect::<Vec<_>>();

                Ok(Cow::Owned(Typ::Union(schema::Union { types })))
            }

            Typ::Builtin(_) => Ok(Cow::Borrowed(typ)),

            Typ::Literal(_) => Ok(Cow::Borrowed(typ)),

            Typ::Pointer(ptr) => {
                let base = ptr.base.as_ref().context("pointer without base")?;
                let typ = base.typ.as_ref().context("base without type")?;
                self.resolve(typ)
            }

            Typ::TypeParameter(param) => {
                let idx = param.param_idx as usize;
                let typ = &self.resolved_args[idx];
                Ok(typ.clone())
            }

            Typ::Config(_cfg) => {
                anyhow::bail!("config types are not supported")
            }
        }
    }

    fn resolve_types(&self, types: &'a [schema::Type]) -> anyhow::Result<Vec<Cow<'a, Typ>>> {
        types
            .iter()
            .map(|typ| {
                let typ = typ.typ.as_ref().context("type without type")?;
                self.resolve(typ)
            })
            .collect()
    }
}

pub struct ReqSchemaUnderConstruction {
    pub methods: Vec<Method>,
    pub schema: SchemaUnderConstruction,
}

impl ReqSchemaUnderConstruction {
    pub fn build(self, reg: &Arc<jsonschema::Registry>) -> anyhow::Result<ReqSchema> {
        Ok(ReqSchema {
            methods: self.methods,
            schema: self.schema.build(reg)?,
        })
    }
}

pub struct ReqSchema {
    pub methods: Vec<Method>,
    pub schema: Schema,
}

pub struct HandshakeSchemaUnderConstruction {
    pub parse_data: bool,
    pub schema: SchemaUnderConstruction,
}

impl HandshakeSchemaUnderConstruction {
    pub fn build(self, reg: &Arc<jsonschema::Registry>) -> anyhow::Result<HandshakeSchema> {
        Ok(HandshakeSchema {
            parse_data: self.parse_data,
            schema: self.schema.build(reg)?,
        })
    }
}
pub struct HandshakeSchema {
    pub parse_data: bool,
    pub schema: Schema,
}

/// Computes the handshake encoding for the given rpc.
pub fn handshake_encoding(
    registry_builder: &mut jsonschema::Builder,
    meta: &meta::Data,
    rpc: &meta::Rpc,
) -> anyhow::Result<Option<HandshakeSchemaUnderConstruction>> {
    // Only streaming endpoints have a handshake schema
    if !rpc.streaming_request && !rpc.streaming_response {
        return Ok(None);
    }

    let default_path = meta::Path {
        segments: vec![meta::PathSegment {
            value: format!("{}.{}", rpc.service_name, rpc.name),
            r#type: SegmentType::Literal as i32,
            value_type: meta::path_segment::ParamType::String as i32,
            validation: None,
        }],
        r#type: meta::path::Type::Url as i32,
    };
    let rpc_path = rpc.path.as_ref().unwrap_or(&default_path);

    let Some(handshake_schema) = &rpc.handshake_schema else {
        let parse_data = rpc.path.as_ref().is_some_and(|path| {
            path.segments
                .iter()
                .any(|segment| segment.r#type() != SegmentType::Literal)
        });

        return Ok(Some(HandshakeSchemaUnderConstruction {
            parse_data,
            schema: SchemaUnderConstruction {
                combined: None,
                body: None,
                query: None,
                header: None,
                cookie: None,
                rpc_path: Some(rpc_path.clone()),
            },
        }));
    };

    let mut config = EncodingConfig {
        meta,
        registry_builder,
        default_loc: Some(DefaultLoc::Query),
        rpc_path: Some(rpc_path),
        supports_body: false,
        supports_query: true,
        supports_header: true,
        supports_path: true,
    };

    let schema = config.compute(handshake_schema)?;

    Ok(Some(HandshakeSchemaUnderConstruction {
        parse_data: true,
        schema,
    }))
}

/// Computes the request encoding for the given rpc.
pub fn request_encoding(
    registry_builder: &mut jsonschema::Builder,
    meta: &meta::Data,
    rpc: &meta::Rpc,
) -> anyhow::Result<Vec<ReqSchemaUnderConstruction>> {
    // Streaming request can only have a body schema
    if rpc.streaming_request || rpc.streaming_response {
        let Some(request_schema) = &rpc.request_schema else {
            return Ok(vec![ReqSchemaUnderConstruction {
                methods: vec![Method::GET],
                schema: SchemaUnderConstruction {
                    combined: None,
                    body: None,
                    query: None,
                    header: None,
                    cookie: None,
                    rpc_path: None,
                },
            }]);
        };

        let mut config = EncodingConfig {
            meta,
            registry_builder,
            default_loc: Some(DefaultLoc::Body),
            rpc_path: None,
            supports_body: true,
            supports_query: false,
            supports_header: false,
            supports_path: false,
        };

        let schema = config.compute(request_schema)?;
        return Ok(vec![ReqSchemaUnderConstruction {
            methods: vec![Method::GET],
            schema,
        }]);
    }

    // Compute the set of methods.
    let methods = {
        let methods: anyhow::Result<Vec<Method>> = rpc
            .http_methods
            .iter()
            .map(|m| Method::try_from(m.as_str()))
            .collect();
        methods.context("unable to parse http methods")?
    };

    let default_path = meta::Path {
        segments: vec![meta::PathSegment {
            value: format!("{}.{}", rpc.service_name, rpc.name),
            r#type: SegmentType::Literal as i32,
            value_type: meta::path_segment::ParamType::String as i32,
            validation: None,
        }],
        r#type: meta::path::Type::Url as i32,
    };
    let rpc_path = rpc.path.as_ref().unwrap_or(&default_path);

    // If there is no request schema, use the same encoding for all methods.
    let Some(request_schema) = &rpc.request_schema else {
        return Ok(vec![ReqSchemaUnderConstruction {
            methods,
            schema: SchemaUnderConstruction {
                combined: None,
                body: None,
                query: None,
                header: None,
                cookie: None,
                rpc_path: Some(rpc_path.clone()),
            },
        }]);
    };

    let mut schemas = Vec::new();

    for default_loc in split_by_loc(&methods) {
        let mut config = EncodingConfig {
            meta,
            registry_builder,
            default_loc: Some(default_loc.0),
            rpc_path: Some(rpc_path),
            supports_body: true,
            supports_query: true,
            supports_header: true,
            supports_path: true,
        };
        let schema = config.compute(request_schema)?;
        schemas.push(ReqSchemaUnderConstruction {
            methods: default_loc.1.clone(),
            schema,
        });
    }

    Ok(schemas)
}

/// Computes the request encoding for the given rpc.
pub fn response_encoding(
    registry_builder: &mut jsonschema::Builder,
    meta: &meta::Data,
    rpc: &meta::Rpc,
) -> anyhow::Result<SchemaUnderConstruction> {
    let Some(response_schema) = &rpc.response_schema else {
        return Ok(SchemaUnderConstruction {
            combined: None,
            body: None,
            query: None,
            header: None,
            cookie: None,
            rpc_path: None,
        });
    };

    let mut config = EncodingConfig {
        meta,
        registry_builder,
        default_loc: Some(DefaultLoc::Body),
        rpc_path: None,
        supports_body: true,
        supports_query: false,
        supports_header: true,
        supports_path: false,
    };
    config.compute(response_schema)
}

fn split_by_loc(methods: &[Method]) -> impl Iterator<Item = (DefaultLoc, Vec<Method>)> {
    let mut locs = HashMap::new();
    for m in methods {
        let loc = if m.supports_body() {
            DefaultLoc::Body
        } else {
            DefaultLoc::Query
        };
        locs.entry(loc).or_insert(Vec::new()).push(*m);
    }

    locs.into_iter()
}
