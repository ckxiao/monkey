package monkey

/*
#include "monkey.h"
*/
import "C"
import (
	"reflect"
	"runtime"
	"unsafe"
)

type JsObjectFunc func(obj *Object, name string, argv []*Value) *Value

// JavaScript Object
type Object struct {
	cx      *Context
	obj     *C.JSObject
	gval    interface{}
	funcs   map[string]JsObjectFunc
	getters map[string]JsPropertyGetter
	setters map[string]JsPropertySetter
}

// Add the JSObject to the garbage collector's root set.
// See: https://developer.mozilla.org/en-US/docs/Mozilla/Projects/SpiderMonkey/JSAPI_reference/JS_AddRoot
func newObject(cx *Context, obj *C.JSObject, gval interface{}) *Object {
	gobj := (*Object)(C.JS_GetPrivate(cx.jscx, obj))
	if gobj != nil {
		return gobj
	}

	result := &Object{cx, obj, gval, nil, nil, nil}

	C.JS_AddObjectRoot(cx.jscx, &result.obj)

	runtime.SetFinalizer(result, func(o *Object) {
		C.JS_RemoveObjectRoot(o.cx.jscx, &o.obj)
	})

	// User defined property and function object use this to find callback.
	C.JS_SetPrivate(cx.jscx, result.obj, unsafe.Pointer(result))

	return result
}

func (o *Object) Runtime() *Runtime {
	return o.cx.rt
}

func (o *Object) Context() *Context {
	return o.cx
}

func (o *Object) ToGo() interface{} {
	keys := o.Keys()
	ret := make(map[string]interface{}, len(keys))
	for _, key := range keys {
		value := o.GetProperty(key)

		// TODO: warp
		// ret[key] = func(argv []interface{}) {
		//     argv => argv2
		//     value.Call(argv2)
		// }
		if value.IsFunction() {
			continue
		}

		ret[key] = value.ToGo()
	}
	return ret
}

func (o *Object) GetPrivate() interface{} {
	return o.gval
}

func (o *Object) SetPrivate(gval interface{}) {
	o.gval = gval
}

func (o *Object) ToValue() *Value {
	return newValue(o.cx, C.OBJECT_TO_JSVAL(o.obj))
}

func (o *Object) GetProperty(name string) *Value {
	o.cx.rt.lock()
	defer o.cx.rt.unlock()

	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	var rval C.jsval
	if C.JS_GetProperty(o.cx.jscx, o.obj, cname, &rval) == C.JS_TRUE {
		return newValue(o.cx, rval)
	}

	return nil
}

func (o *Object) Keys() []string {
	o.cx.rt.lock()
	defer o.cx.rt.unlock()

	ids := C.JS_Enumerate(o.cx.jscx, o.obj)
	if ids == nil {
		panic("enumerate failed")
	}
	defer C.JS_free(o.cx.jscx, unsafe.Pointer(ids))

	keys := make([]string, ids.length)
	head := unsafe.Pointer(&ids.vector[0])

	sl := &reflect.SliceHeader{
		uintptr(head), len(keys), len(keys),
	}
	vector := *(*[]C.jsid)(unsafe.Pointer(sl))
	for i := 0; i < len(keys); i++ {
		id := vector[i]
		ckey := C.JS_EncodeString(o.cx.jscx, C.JSID_TO_STRING(id))
		gkey := C.GoString(ckey)
		C.JS_free(o.cx.jscx, unsafe.Pointer(ckey))
		keys[i] = gkey
	}

	return keys
}

func (o *Object) SetProperty(name string, v *Value) bool {
	o.cx.rt.lock()
	defer o.cx.rt.unlock()

	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	return C.JS_SetProperty(o.cx.jscx, o.obj, cname, &v.val) == C.JS_TRUE
}

type JsPropertyAttrs uint

// Property Attributes
const (
	JSPROP_ENUMERATE = C.JSPROP_ENUMERATE // The property is visible to JavaScript for…in and for each…in loops.
	JSPROP_READONLY  = C.JSPROP_READONLY  // The property's value cannot be set.
	JSPROP_PERMANENT = C.JSPROP_PERMANENT // The property cannot be deleted.
)

type JsPropertyGetter func(o *Object, name string) *Value
type JsPropertySetter func(o *Object, name string, v *Value)

//export call_go_getter
func call_go_getter(obj unsafe.Pointer, name *C.char, val *C.jsval) C.JSBool {
	o := (*Object)(obj)
	if o.getters != nil {
		if v := o.getters[C.GoString(name)](o, C.GoString(name)); v != nil {
			*val = v.val
			return C.JS_TRUE
		}
	}
	return C.JS_FALSE
}

//export call_go_setter
func call_go_setter(obj unsafe.Pointer, name *C.char, val *C.jsval) C.JSBool {
	o := (*Object)(obj)
	if o.setters != nil {
		o.setters[C.GoString(name)](o, C.GoString(name), newValue(o.cx, *val))
		return C.JS_TRUE
	}
	return C.JS_FALSE
}

func (o *Object) DefineProperty(name string, value *Value, getter JsPropertyGetter, setter JsPropertySetter, attrs JsPropertyAttrs) bool {
	o.cx.rt.lock()
	defer o.cx.rt.unlock()

	if C.JS_IsArrayObject(o.cx.jscx, o.obj) == C.JS_TRUE {
		panic("Could't define property on array.")
	}

	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	var r C.JSBool

	if getter != nil && setter != nil {
		r = C.JS_DefineProperty(o.cx.jscx, o.obj, cname, value.val, C.the_go_getter_callback, C.the_go_setter_callback, C.uintN(uint(attrs))|C.JSPROP_SHARED)
	} else if getter != nil && setter == nil {
		r = C.JS_DefineProperty(o.cx.jscx, o.obj, cname, value.val, C.the_go_getter_callback, nil, C.uintN(uint(attrs)))
	} else if getter == nil && setter != nil {
		r = C.JS_DefineProperty(o.cx.jscx, o.obj, cname, value.val, nil, C.the_go_setter_callback, C.uintN(uint(attrs)))
	} else {
		panic("The getter and setter both nil")
	}

	if r == C.JS_TRUE {
		if getter != nil {
			if o.getters == nil {
				o.getters = make(map[string]JsPropertyGetter)
			}
			o.getters[name] = getter
		}

		if setter != nil {
			if o.setters == nil {
				o.setters = make(map[string]JsPropertySetter)
			}
			o.setters[name] = setter
		}

		return true
	}

	return false
}

//export call_go_obj_func
func call_go_obj_func(op unsafe.Pointer, name *C.char, argc C.uintN, vp *C.jsval) C.JSBool {
	var o = (*Object)(op)

	var argv = make([]*Value, int(argc))

	for i := 0; i < len(argv); i++ {
		argv[i] = newValue(o.cx, C.GET_ARGV(o.cx.jscx, vp, C.int(i)))
	}

	var gname = C.GoString(name)
	var result = o.funcs[gname](o, gname, argv)

	if result != nil {
		C.SET_RVAL(o.cx.jscx, vp, result.val)
		return C.JS_TRUE
	}

	return C.JS_FALSE
}

// Define a function into object
// @name     The function name
// @callback The function implement
func (o *Object) DefineFunction(name string, callback JsObjectFunc) bool {
	o.cx.rt.lock()
	defer o.cx.rt.unlock()

	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	if C.JS_DefineFunction(o.cx.jscx, o.obj, cname, C.the_go_obj_func_callback, 0, 0) == nil {
		return false
	}

	if o.funcs == nil {
		o.funcs = make(map[string]JsObjectFunc)
	}

	o.funcs[name] = callback

	return true
}

/*
Utilities
*/

func (o *Object) GetInt(name string) (int32, bool) {
	if v := o.GetProperty(name); v != nil {
		return v.ToInt()
	}
	return 0, false
}

func (o *Object) SetInt(name string, v int32) bool {
	return o.SetProperty(name, o.cx.Int(v))
}

func (o *Object) GetNumber(name string) (float64, bool) {
	if v := o.GetProperty(name); v != nil {
		return v.ToNumber()
	}
	return 0, false
}

func (o *Object) SetNumber(name string, v float64) bool {
	return o.SetProperty(name, o.cx.Number(v))
}

func (o *Object) GetBoolean(name string) (bool, bool) {
	if v := o.GetProperty(name); v != nil {
		return v.ToBoolean()
	}
	return false, false
}

func (o *Object) SetBoolean(name string, v bool) bool {
	return o.SetProperty(name, o.cx.Boolean(v))
}

func (o *Object) GetString(name string) (string, bool) {
	if v := o.GetProperty(name); v != nil {
		return v.ToString(), true
	}
	return "", false
}

func (o *Object) SetString(name string, v string) bool {
	return o.SetProperty(name, o.cx.String(v))
}

func (o *Object) GetObject(name string) *Object {
	if v := o.GetProperty(name); v != nil {
		return v.ToObject()
	}
	return nil
}

func (o *Object) SetObject(name string, o2 *Object) bool {
	return o.SetProperty(name, o2.ToValue())
}

func (o *Object) GetArray(name string) *Array {
	if v := o.GetProperty(name); v != nil {
		return v.ToArray()
	}
	return nil
}

func (o *Object) SetArray(name string, o2 *Array) bool {
	return o.SetProperty(name, o2.ToValue())
}
