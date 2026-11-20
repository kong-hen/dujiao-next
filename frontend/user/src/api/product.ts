import { api } from './client'

// 仅公开商品列表/详情在已登录时附带可选 Bearer Token，供后端识别会员等级并返回含成本价托底保护的会员价，
// 不影响游客订单、登录等其他接口，也不会触发 userApi 的 401 强制登出。
const optionalAuthHeaders = (): Record<string, string> | undefined => {
    const token = typeof localStorage !== 'undefined' ? localStorage.getItem('user_token') : null
    return token ? { Authorization: `Bearer ${token}` } : undefined
}

export const productAPI = {
    list: (params?: any) => api.get('/public/products', { params, headers: optionalAuthHeaders() }),
    detail: (slug: string) => api.get(`/public/products/${slug}`, { headers: optionalAuthHeaders() }),
}

export const postAPI = {
    list: (params?: any) => api.get('/public/posts', { params }),
    detail: (slug: string) => api.get(`/public/posts/${slug}`),
}

export const bannerAPI = {
    list: (params?: any) => api.get('/public/banners', { params }),
}

export const categoryAPI = {
    list: (params?: any) => api.get('/public/categories', { params }),
}

export const memberLevelAPI = {
    list: () => api.get('/public/member-levels'),
}
